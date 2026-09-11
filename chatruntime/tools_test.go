package chatruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/goai/tool"
)

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

func TestCapabilitiesRejectAliasCollisions(t *testing.T) {
	def := capability.Definition{Path: capability.Local(capability.Tool, "", "lookup"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := validateCapabilities([]capability.Definition{def, def}); err == nil {
		t.Fatal("duplicate catalog accepted")
	}
}
