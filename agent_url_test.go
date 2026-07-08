package agentsdk

import (
	"context"
	"errors"
	"testing"
)

func TestAgentURL(t *testing.T) {
	a, _ := testAgent(t)

	if got, err := a.AgentURL(); !errors.Is(err, ErrAgentURLUnavailable) || got != "" {
		t.Fatalf("AgentURL before sync = %q, %v; want ErrAgentURLUnavailable", got, err)
	}

	a.applySyncResponse(SyncResponse{PromptData: PromptData{AgentRouteURL: "https://todo.example.com"}})

	got, err := a.AgentURL()
	if err != nil {
		t.Fatalf("AgentURL after sync: %v", err)
	}
	if got != "https://todo.example.com" {
		t.Fatalf("AgentURL after sync = %q", got)
	}
}

func TestAgentURLFromContext(t *testing.T) {
	if got, err := AgentURLFromContext(context.Background()); !errors.Is(err, ErrAgentURLUnavailable) || got != "" {
		t.Fatalf("AgentURLFromContext without agent = %q, %v; want ErrAgentURLUnavailable", got, err)
	}

	a, _ := testAgent(t)
	a.applySyncResponse(SyncResponse{PromptData: PromptData{AgentRouteURL: "https://todo.example.com"}})
	r := newRun(a, "run-1", "", "", context.Background())

	got, err := AgentURLFromContext(contextWithRun(context.Background(), r))
	if err != nil {
		t.Fatalf("AgentURLFromContext: %v", err)
	}
	if got != "https://todo.example.com" {
		t.Fatalf("AgentURLFromContext = %q", got)
	}
}
