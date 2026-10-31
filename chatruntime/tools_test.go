package chatruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/goai/tool"
)

func TestJavaScriptModeAdvertisesOnlyRunJS(t *testing.T) {
	tools := (&runtime{input: Input{}}).tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools.Names())
	}
	runJS, ok := tools["run_js"]
	if !ok {
		t.Fatalf("tools = %v, want run_js", tools.Names())
	}
	for _, want := range []string{"exact fully qualified names", "Bindings exist only inside JavaScript", "run_js_file when available"} {
		if !strings.Contains(runJS.Description, want) {
			t.Errorf("run_js description missing %q: %s", want, runJS.Description)
		}
	}
	if !strings.Contains(string(runJS.InputSchema), `"code"`) || strings.Contains(string(runJS.InputSchema), "tools.") {
		t.Fatalf("run_js input schema = %s", runJS.InputSchema)
	}
}

type testBackend func(context.Context, Invocation) (tool.Result, error)

func (f testBackend) Invoke(ctx context.Context, in Invocation) (tool.Result, error) {
	return f(ctx, in)
}

func TestInvokerRejectsUnavailableAndInvalidArguments(t *testing.T) {
	def := capability.Definition{Path: capability.Local(capability.Tool, "", "lookup"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object"}`)}
	for _, tc := range []struct{ name, id, args string }{
		{"unknown", "tool//hidden", `[{}]`},
		{"not array", def.Path.ID(), `{}`},
		{"missing object", def.Path.ID(), `[]`},
		{"extra arguments", def.Path.ID(), `[{},{}]`},
		{"null", def.Path.ID(), `[null]`},
		{"string", def.Path.ID(), `["path"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			i := &invoker{catalog: []capability.Definition{def}, backend: testBackend(func(context.Context, Invocation) (tool.Result, error) { called = true; return tool.Result{}, nil })}
			if _, err := i.Invoke(t.Context(), tc.id, json.RawMessage(tc.args)); err == nil || called {
				t.Fatalf("error=%v backend called=%v", err, called)
			}
		})
	}
}

func TestInvokerPreservesJSONString(t *testing.T) {
	def := capability.Definition{Path: capability.Local(capability.Air, "", "file_read"), Target: capability.App, TextOutput: true}
	i := &invoker{catalog: []capability.Definition{def}, backend: testBackend(func(context.Context, Invocation) (tool.Result, error) {
		return tool.Result{Output: `{"value":42}`}, nil
	})}
	value, err := i.Invoke(t.Context(), def.Path.ID(), json.RawMessage(`[{"path":"/tmp/data.json"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil || text != `{"value":42}` {
		t.Fatalf("text=%q err=%v", text, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := i.Invoke(ctx, def.Path.ID(), json.RawMessage(`[{}]`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestInvokerDecodesFixedFileResults(t *testing.T) {
	outputs := map[string]string{
		"file_stat":       `{"path":"tmp/result","filename":"result","contentType":"text/plain","size":0,"lastModified":"2026-10-31T00:00:00Z"}`,
		"file_list":       `null`,
		"file_exists":     `{"exists":false}`,
		"file_delete":     `{"deleted":true}`,
		"file_read_bytes": `{"size":1,"base64":"YQ=="}`,
		"file_sed":        `{"inline":true,"content":"","size":0}`,
		"file_encode":     `{"inline":false,"savedTo":"tmp/result","size":12}`,
	}
	for _, def := range capability.Fixed() {
		output, ok := outputs[def.Path.CanonicalOperation()]
		if !ok {
			continue
		}
		t.Run(def.Path.CanonicalOperation(), func(t *testing.T) {
			i := &invoker{catalog: []capability.Definition{def}, backend: testBackend(func(context.Context, Invocation) (tool.Result, error) {
				return tool.Result{Output: output, Title: "transport title", Metadata: map[string]any{"transport": true}}, nil
			})}
			value, err := i.Invoke(t.Context(), def.Path.ID(), json.RawMessage(`[{}]`))
			if err != nil {
				t.Fatal(err)
			}
			if string(value) != output {
				t.Fatalf("decoded result=%s, want %s", value, output)
			}
		})
	}
}

func TestCapabilitiesRejectAliasCollisions(t *testing.T) {
	def := capability.Definition{Path: capability.Local(capability.Tool, "", "lookup"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := validateCapabilities([]capability.Definition{def, def}); err == nil {
		t.Fatal("duplicate catalog accepted")
	}
}
