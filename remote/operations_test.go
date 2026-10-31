package remote

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentListAndContinuation(t *testing.T) {
	base := "/api/v1/agents/" + appID + "/deployments/p-random"
	var continued bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Error("missing operator credential")
		}
		switch r.URL.Path {
		case base:
			deploymentResponse(w)
		case base + "/agents/task/runs":
			if r.Method != http.MethodGet {
				t.Error(r.Method)
			}
			fmt.Fprintf(w, `{"runs":[{"id":%q,"definition":"task","sessionId":%q,"status":"succeeded"}]}`, runID, ownerID)
		case base + "/agents/task/sessions/" + ownerID + "/continue":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || !strings.Contains(string(body), `"prompt":"Next step"`) || !strings.Contains(string(body), `"requestId":"stable-request"`) {
				t.Errorf("continuation: %s %s", r.Method, body)
			}
			continued = true
			fmt.Fprintf(w, `{"run":{"id":%q,"definition":"task","sessionId":%q,"status":"queued"},"created":true}`, runID, ownerID)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := client.ListAgents(t.Context(), "task")
	if err != nil || len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("list: %+v %v", runs, err)
	}
	run, err := client.ContinueAgent(t.Context(), "task", ownerID, "stable-request", "", "Next step")
	if err != nil || run == nil || run.ID != runID || !continued {
		t.Fatalf("continue: %+v %v", run, err)
	}
}
