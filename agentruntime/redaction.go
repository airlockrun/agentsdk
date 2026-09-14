package agentruntime

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/airlockrun/sol/session"
)

func redactMessages(msgs []session.Message, redact func(string) string) []session.Message {
	result := slices.Clone(msgs)
	for i := range result {
		m := &result[i]
		m.Content = redact(m.Content)
		m.Parts = slices.Clone(m.Parts)
		for j := range m.Parts {
			p := &m.Parts[j]
			p.Text = redact(p.Text)
			if p.Tool == nil {
				continue
			}
			t := *p.Tool
			p.Tool = &t
			if t.Input != "" {
				t.Input = string(redactJSON(json.RawMessage(t.Input), redact))
			}
			switch t.OutputType {
			case "json", "error-json":
				t.Output = string(redactJSON(json.RawMessage(t.Output), redact))
			case "content":
				// Content outputs can contain binary data and provider references;
				// only text items are subject to substring replacement.
				var items []map[string]json.RawMessage
				if err := json.Unmarshal([]byte(t.Output), &items); err != nil {
					panic(err)
				}
				for _, item := range items {
					if string(item["type"]) == `"text"` {
						item["text"] = redactJSON(item["text"], redact)
					}
				}
				raw, err := json.Marshal(items)
				if err != nil {
					panic(err)
				}
				t.Output = string(raw)
			default:
				t.Output = redact(t.Output)
			}
		}
	}
	return result
}

func redactJSON(raw json.RawMessage, redact func(string) string) json.RawMessage {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		panic(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return redact(x)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, child := range x {
				out[redact(k)] = walk(child)
			}
			return out
		}
		return v
	}
	out, err := json.Marshal(walk(value))
	if err != nil {
		panic(err)
	}
	return out
}
