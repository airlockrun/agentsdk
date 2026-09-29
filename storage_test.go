package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestFileRefAndManualIndex(t *testing.T) {
	ref := FileRef{ID: "entry", ContentID: "content", Name: "report.txt", Path: "/apps/app/docs/report.txt", ContentType: "text/plain", Size: 6}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/storage/ref", func(w http.ResponseWriter, r *http.Request) {
		var request wire.StorageFileRefRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Path != "docs/report.txt" {
			t.Errorf("ref request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(ref)
	})
	mux.HandleFunc("POST /api/agent/storage/index", func(w http.ResponseWriter, r *http.Request) {
		var request wire.StorageSetIndexRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.File != ref || request.Text != "quarterly revenue" {
			t.Errorf("index request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(ref)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	agent := &Agent{phase: agentRunning, client: newAirlockClient(server.URL, "token", server.Client())}

	got, err := agent.StatFileRef(context.Background(), "docs/report.txt")
	if err != nil || got != ref {
		t.Fatalf("StatFileRef() = %+v, %v", got, err)
	}
	got, err = agent.SetFileIndex(context.Background(), got, "quarterly revenue")
	if err != nil || got != ref {
		t.Fatalf("SetFileIndex() = %+v, %v", got, err)
	}
}
