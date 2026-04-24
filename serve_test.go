package agentsdk

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "test_tool",
		Description: "Test tool.",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	a.handleHealth(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status string   `json:"status"`
		Tools  []string `json:"tools"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "ok" {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
	if len(resp.Tools) != 1 || resp.Tools[0] != "test_tool" {
		t.Fatalf("expected [test_tool], got %v", resp.Tools)
	}
}
