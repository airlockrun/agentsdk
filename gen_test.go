package agentsdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
)

// prepareGenInput fills a nil chat model from the agent's text-capability proxy
// and leaves an explicitly-set model untouched.
func TestPrepareGenInput_ModelFill(t *testing.T) {
	a, _ := testAgent(t)
	r := newRun(a, "gen-fill", "", "", context.Background())
	ctx := contextWithRun(context.Background(), r)

	t.Run("nil model is filled", func(t *testing.T) {
		in := stream.Input{}
		a.prepareGenInput(ctx, &in, r)
		if in.Model == nil {
			t.Fatal("expected a proxy chat model to be filled in")
		}
	})

	t.Run("explicit model is preserved", func(t *testing.T) {
		want := a.proxyLLM(ctx, "", CapVision)
		in := stream.Input{Model: want}
		a.prepareGenInput(ctx, &in, r)
		if in.Model != want {
			t.Fatal("explicit model must not be overwritten")
		}
	})
}

// A wrapped tool's Execute runs under the run-scoped context, so a tool passed
// to GenerateText/StreamText can resolve the run exactly as a RegisterTool'd
// tool does inside the VM.
func TestPrepareGenInput_ToolSeesRun(t *testing.T) {
	a, _ := testAgent(t)
	r := newRun(a, "gen-run-42", "", "", context.Background())
	ctx := contextWithRun(context.Background(), r)

	var seenID string
	probe := tool.Typed[struct{}, struct{}]("probe").
		Description("captures the run id from ctx").
		Execute(func(ctx context.Context, _ struct{}) (struct{}, error) {
			if got := runFromContext(ctx); got != nil {
				seenID = got.id
			}
			return struct{}{}, nil
		}).Build()

	in := stream.Input{Tools: tool.Set{"probe": probe}}
	a.prepareGenInput(ctx, &in, r)

	// The wrapped tool is a copy; the original must stay un-wrapped.
	if _, err := in.Tools["probe"].Execute(context.Background(), json.RawMessage(`{}`), tool.CallOptions{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if seenID != "gen-run-42" {
		t.Fatalf("tool saw run id %q, want gen-run-42", seenID)
	}
}

// Provider/no-execute tools have no Execute to wrap and pass through unchanged.
func TestWrapToolWithRun_PassthroughNoExecute(t *testing.T) {
	a, _ := testAgent(t)
	r := newRun(a, "gen-pt", "", "", context.Background())
	provider := tool.Tool{Type: string(tool.KindProviderDefined), Name: "web_search", ProviderID: "openai.web_search"}
	got := wrapToolWithRun(provider, r)
	if got.Execute != nil {
		t.Fatal("provider tool should pass through with nil Execute")
	}
	_ = a
}
