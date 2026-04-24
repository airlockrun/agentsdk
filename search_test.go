package agentsdk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/sol/websearch"
)

func TestProxySearchClient(t *testing.T) {
	// Mock Airlock search endpoint.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/agent/search" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}

		var req websearch.Request
		json.NewDecoder(r.Body).Decode(&req)
		if req.Query != "test query" {
			t.Errorf("expected query 'test query', got %q", req.Query)
		}

		json.NewEncoder(w).Encode(websearch.Response{
			Results: []websearch.Result{
				{Title: "Result 1", URL: "https://example.com", Snippet: "A test result"},
			},
			Provider: "brave",
		})
	}))
	defer server.Close()

	client := &proxySearchClient{
		client: &airlockClient{
			baseURL:    server.URL,
			token:      "test-token",
			http: server.Client(),
		},
	}

	resp, err := client.Search(t.Context(), websearch.Request{Query: "test query", Count: 5})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].Title != "Result 1" {
		t.Errorf("expected title 'Result 1', got %q", resp.Results[0].Title)
	}
	if resp.Provider != "brave" {
		t.Errorf("expected provider 'brave', got %q", resp.Provider)
	}
}

func TestProxySearchClientNotConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "web search not configured"})
	}))
	defer server.Close()

	client := &proxySearchClient{
		client: &airlockClient{
			baseURL:    server.URL,
			token:      "test-token",
			http: server.Client(),
		},
	}

	_, err := client.Search(t.Context(), websearch.Request{Query: "test"})
	if err == nil {
		t.Fatal("expected error for unconfigured search, got nil")
	}
}
