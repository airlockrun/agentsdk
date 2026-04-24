package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/goai/message"
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
