package agentsdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvVarHandleGetFallsBackToDefaultOnUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/env-vars/region" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":"env var has no configured value"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}, sensitiveSet: make(map[string]struct{}), phase: agentRunning}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &EnvVarHandle{slug: "region", defaultValue: "us-east-1", agent: a}

	got, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got != "us-east-1" {
		t.Fatalf("Get = %q, want default", got)
	}
}

func TestEnvVarHandleGetUnsetWithoutDefaultErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"env var has no configured value"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}, sensitiveSet: make(map[string]struct{}), phase: agentRunning}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &EnvVarHandle{slug: "region", agent: a}

	if _, err := h.Get(context.Background()); err == nil {
		t.Fatal("Get returned nil error for unset env var without default")
	}
}
