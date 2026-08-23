package agentsdk

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAirlockClientRequestHeaders(t *testing.T) {
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client := newAirlockClient(server.URL, "secret", server.Client())
	run := &run{id: "bound-run"}
	response, err := client.do(contextWithRun(context.Background(), run), http.MethodPost, "/bound", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	bound := <-requests
	if got := bound.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := bound.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := bound.Header.Get("X-Airlock-Run-ID"); got != "bound-run" {
		t.Fatalf("X-Airlock-Run-ID = %q", got)
	}

	inbound := httptest.NewRequest(http.MethodGet, "https://agent.test/route", nil)
	inbound.Header.Set("X-Airlock-Run-ID", "inbound-run")
	ctx := withCaller(inbound.Context(), caller{RunID: "caller-run"})
	response, err = client.do(ctx, http.MethodGet, "/unbound", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	unbound := <-requests
	if got := unbound.Header.Get("X-Airlock-Run-ID"); got != "" {
		t.Fatalf("X-Airlock-Run-ID = %q, want empty", got)
	}
}
