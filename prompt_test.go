package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/goai/message"
	sol "github.com/airlockrun/sol"
)

type greetIn struct {
	Name string `json:"name"`
}

type greetOut struct {
	Greeting string `json:"greeting"`
}

func TestPromptHandler(t *testing.T) {
	a, mock := testAgent(t)
	_ = mock

	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "greet",
		Description: "Returns a greeting.",
		Execute: func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "Hello, " + in.Name + "!"}, nil
		},
		Access: AccessUser,
	})

	input := PromptInput{
		Messages: []message.Message{
			message.NewUserMessage("say hello to World"),
		},
	}
	body, _ := json.Marshal(input)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/prompt", bytes.NewReader(body))
	r.Header.Set("X-Run-ID", "run-prompt-1")

	handlePrompt(a)(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should have NDJSON response.
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"type"`) {
		t.Fatalf("expected NDJSON events, got: %s", respBody)
	}

	// Should have recorded run completion.
	completeReqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(completeReqs) != 1 {
		t.Fatalf("expected 1 complete request, got %d", len(completeReqs))
	}
}

// TestResumeInProcessChild_EarlyReturns locks the (string,
// *bus.ErrDelegatedSuspend) contract on the non-suspending paths: a
// failure must NOT yield a re-suspension envelope (a non-nil there would
// wrongly re-suspend the parent). The re-suspension propagation itself
// (subagent re-suspends, or its nested delegate re-suspends) is verified
// end-to-end — it needs an LLM-driven Sol run or a programmable MCP
// endpoint, the same reason the analogous A2A fix has no unit test.
func TestResumeInProcessChild_EarlyReturns(t *testing.T) {
	a, _ := testAgent(t)
	ew := newEventWriter(httptest.NewRecorder())

	t.Run("decode error", func(t *testing.T) {
		text, susp := resumeInProcessChild(context.Background(), a, "run-1",
			sol.RunnerOptions{}, json.RawMessage("not json"), true, "", ew)
		if susp != nil {
			t.Fatalf("decode error must not re-suspend, got %+v", susp)
		}
		if !strings.HasPrefix(text, "Error: decode in-process child:") {
			t.Fatalf("unexpected text: %q", text)
		}
	})

	t.Run("unknown subagent factory", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"agentName": "no-such-subagent-xyz", "messages": []any{}})
		text, susp := resumeInProcessChild(context.Background(), a, "run-1",
			sol.RunnerOptions{}, raw, true, "", ew)
		if susp != nil {
			t.Fatalf("missing factory must not re-suspend, got %+v", susp)
		}
		if !strings.HasPrefix(text, "Error: subagent type not found on resume:") {
			t.Fatalf("unexpected text: %q", text)
		}
	})
}
