package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/session"
)

func TestRedactMessagesPreservesJSONNumbersFilesAndMetadata(t *testing.T) {
	for _, output := range []message.ToolResultOutput{
		message.JSONOutput{Value: json.RawMessage(`{"key":"secret","n":9007199254740993}`)},
		message.ErrorJSONOutput{Value: json.RawMessage(`{"key":"secret","n":9007199254740993}`)},
		message.ContentOutput{Value: []message.ToolContentItem{{Type: "text", Text: "secret"}, {Type: "image-data", Data: "secret"}}},
	} {
		t.Run(message.ToolOutcome(output)+message.ToolOutputWire(output), func(t *testing.T) {
			msg := session.FromGoAIMessage(message.NewToolMessage("id", "tool", output))
			msg.ProviderOptions = map[string]any{"opaque": "secret"}
			before, _ := json.Marshal(msg)
			redacted := redactMessages([]session.Message{msg}, func(s string) string { return strings.ReplaceAll(s, "secret", `quote"replacement`) })
			after, _ := json.Marshal(msg)
			if string(before) != string(after) {
				t.Fatal("redaction mutated original")
			}
			if redacted[0].ProviderOptions["opaque"] != "secret" {
				t.Fatal("opaque provider metadata changed")
			}
			if strings.Contains(redacted[0].Parts[0].Tool.Output, `"n":`) && !strings.Contains(redacted[0].Parts[0].Tool.Output, "9007199254740993") {
				t.Fatal("integer precision lost")
			}
			_ = session.MessagesToGoAI(redacted)
		})
	}
}

func TestRunRedactsWholeTextBeforeEmitting(t *testing.T) {
	response := batch(complete("done"))
	response = append([]stream.Event{{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "sec"}}, {Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "ret"}}}, response...)
	in, _ := input(response)
	in.Redactor = func(s string) string { return strings.ReplaceAll(s, "secret", "[redacted]") }
	if _, err := Run(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if strings.Join(in.Sink.(*sink).texts, "") != "[redacted]" {
		t.Fatal(in.Sink.(*sink).texts)
	}
	raw, _ := json.Marshal(in.Store.(*memoryStore).messages)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("persisted unredacted assistant text")
	}
}
