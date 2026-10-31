package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
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
	run := &run{id: "bound-run", invocationToken: "invocation-secret"}
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
	if got := bound.Header.Get(wire.InvocationTokenHeader); got != run.invocationToken {
		t.Fatalf("invocation token = %q", got)
	}

	inbound := httptest.NewRequest(http.MethodGet, "https://agent.test/route", nil)
	inbound.Header.Set("X-Airlock-Run-ID", "inbound-run")
	inbound.Header.Set(wire.InvocationTokenHeader, "untrusted-header")
	ctx := withCallScope(inbound.Context(), callScope{RunID: "caller-run"})
	response, err = client.do(ctx, http.MethodGet, "/unbound", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	unbound := <-requests
	if got := unbound.Header.Get("X-Airlock-Run-ID"); got != "" {
		t.Fatalf("X-Airlock-Run-ID = %q, want empty", got)
	}
	if got := unbound.Header.Get(wire.InvocationTokenHeader); got != "" {
		t.Fatalf("unbound invocation token = %q", got)
	}
}

func TestAirlockClientMaterializesRouteCallbacks(t *testing.T) {
	a, _ := testAgent(t)
	materialized, callbacks := 0, 0
	proof := "host-signed-route"
	token := strings.Repeat("ab", 32)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/run/materialize-route" {
			materialized++
			var request wire.MaterializeRouteRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Proof != proof {
				t.Errorf("route proof: %+v %v", request, err)
			}
			if r.Header.Get(wire.InvocationTokenHeader) != "" {
				t.Error("materialization invented an invocation receipt")
			}
			_ = json.NewEncoder(w).Encode(wire.MaterializeRouteResponse{Runtime: wire.RuntimeContext{RunID: "route-run", InvocationToken: token, Caller: testWireCaller("user", wire.AccessUser)}, Deadline: time.Now().Add(time.Minute), Logging: wire.LoggingOff})
			return
		}
		callbacks++
		if r.Header.Get("X-Airlock-Run-ID") != "route-run" || r.Header.Get(wire.InvocationTokenHeader) != token {
			t.Error("route callback omitted its exact receipt")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer host.Close()
	a.client = newAirlockClient(host.URL, a.token, host.Client())
	ctx := contextWithLazyRun(t.Context(), &lazyRun{agent: a, proof: proof})
	for range 2 {
		response, err := a.client.do(ctx, http.MethodGet, "/api/agent/storage/docs/file.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	if materialized != 1 || callbacks != 2 {
		t.Fatalf("materializations=%d callbacks=%d", materialized, callbacks)
	}
}
