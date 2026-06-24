package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
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

// TestHealthEndpointDBUnavailable verifies that when the agent has a DB
// configured but it can't be reached/authenticated, /health reports 503 —
// so the dispatcher keeps the agent out of rotation instead of routing
// traffic that would 500 on the first query (drifted DB role, DB down).
func TestHealthEndpointDBUnavailable(t *testing.T) {
	// Point at a closed port so the ping fails fast with connection refused.
	t.Setenv("AIRLOCK_DB_URL", "postgres://nope:nope@127.0.0.1:65500/none?sslmode=disable&connect_timeout=2")
	a, _ := testAgent(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	a.handleHealth(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when DB unreachable, got %d", w.Code)
	}
	var resp struct {
		Status string `json:"status"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "db_unavailable" {
		t.Fatalf("expected db_unavailable, got %s", resp.Status)
	}
}
