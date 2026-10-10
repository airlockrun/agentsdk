package mockairlock

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHostedTaskLifecycleRequiresRunAgent(t *testing.T) {
	m, url := New()
	defer m.Close()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/agent/agents/conversion/runs"},
		{"GET", "/api/agent/agents/conversion/runs?contractHash=hash"},
		{"GET", "/api/agent/agents/conversion/runs/run"},
		{"DELETE", "/api/agent/agents/conversion/runs/run"},
		{"POST", "/api/agent/agents/conversion/sessions/session/continue"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, url+tc.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := m.Server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(string(body), "agenttest.RunAgent") {
				t.Fatalf("response = %d %s", resp.StatusCode, body)
			}
		})
	}
	if len(m.Requests()) != 5 {
		t.Fatalf("requests = %d", len(m.Requests()))
	}
}
