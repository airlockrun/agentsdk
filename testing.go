package agentsdk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// MockRequest records a request made to the mock Airlock server.
type MockRequest struct {
	Method string
	Path   string
	Body   []byte
}

// MockAirlock is an httptest server that implements the Airlock agent API.
// Use NewMockAirlock() to create one. Exported for agent developers' tests.
type MockAirlock struct {
	Server   *httptest.Server
	mu       sync.Mutex
	requests []MockRequest

	// LLMResponse is the NDJSON response returned by POST /api/agent/llm/stream.
	// Set this before making prompt requests.
	LLMResponse []byte
}

// NewMockAirlock creates a mock Airlock server and returns it along with the base URL.
func NewMockAirlock() (*MockAirlock, string) {
	m := &MockAirlock{}
	mux := http.NewServeMux()

	// Proxy endpoint.
	mux.HandleFunc("POST /api/agent/proxy/{slug}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Storage endpoints. Use {key...} so multi-segment keys like
	// "tmp/generated/img-abc.png" match (single-segment {key} would 404).
	mux.HandleFunc("PUT /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Write([]byte("mock-file-content"))
	})
	mux.HandleFunc("DELETE /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/agent/storage", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		json.NewEncoder(w).Encode([]StoredFile{})
	})

	// Storage copy endpoint.
	mux.HandleFunc("POST /api/agent/storage/copy", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})

	// Storage info endpoint.
	mux.HandleFunc("POST /api/agent/storage/info", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(StoredFile{
			Key:         "test.txt",
			Size:        42,
			ContentType: "text/plain",
		})
	})

	// Print endpoint (printToUser / topic publish).
	mux.HandleFunc("POST /api/agent/print", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})

	// Topic subscribe/unsubscribe endpoints.
	mux.HandleFunc("POST /api/agent/topic/{slug}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/agent/topic/{slug}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})

	// Files endpoint.
	mux.HandleFunc("GET /api/agent/files/{fileID}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Write([]byte("mock-attachment"))
	})

	// LLM stream endpoint.
	mux.HandleFunc("POST /api/agent/llm/stream", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/x-ndjson")
		if m.LLMResponse != nil {
			w.Write(m.LLMResponse)
		} else {
			// Default: single text response.
			w.Write([]byte(`{"type":"start","data":{}}` + "\n"))
			w.Write([]byte(`{"type":"text-delta","data":{"text":"Hello"}}` + "\n"))
			w.Write([]byte(`{"type":"finish","data":{"finishReason":"stop","usage":{"inputTokens":{"total":10},"outputTokens":{"total":5}}}}` + "\n"))
		}
	})

	// Non-language model endpoints (image, embedding, speech, transcription).
	mux.HandleFunc("POST /api/agent/llm/image", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"images":   []map[string]string{{"base64": "bW9jay1pbWFnZS1kYXRh", "mimeType": "image/png"}},
			"warnings": []string{},
		})
	})
	mux.HandleFunc("POST /api/agent/llm/embedding", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{{"values": []float64{0.1, 0.2, 0.3}, "index": 0}},
			"usage":      map[string]int{"tokens": 5},
		})
	})
	mux.HandleFunc("POST /api/agent/llm/speech", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"audio":    "bW9jay1hdWRpbw==",
			"mimeType": "audio/mpeg",
		})
	})
	mux.HandleFunc("POST /api/agent/llm/transcription", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"text":     "mock transcription",
			"language": "en",
		})
	})

	// Run create endpoint.
	mux.HandleFunc("POST /api/agent/run/create", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CreateRunResponse{RunID: "run-mock-123"})
	})

	// Run complete endpoint.
	mux.HandleFunc("POST /api/agent/run/complete", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})

	// Connection registration.
	mux.HandleFunc("PUT /api/agent/connections/{slug}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})

	// Sync endpoint — returns rendered system prompt.
	mux.HandleFunc("PUT /api/agent/sync", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncResponse{SystemPrompt: "mock system prompt"})
	})

	// Upgrade endpoint.
	mux.HandleFunc("POST /api/agent/upgrade", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusAccepted)
	})

	m.Server = httptest.NewServer(mux)
	return m, m.Server.URL
}

// Requests returns all recorded requests.
func (m *MockAirlock) Requests() []MockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// RequestsByPath returns requests matching the given path prefix.
func (m *MockAirlock) RequestsByPath(prefix string) []MockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MockRequest
	for _, r := range m.requests {
		if len(r.Path) >= len(prefix) && r.Path[:len(prefix)] == prefix {
			out = append(out, r)
		}
	}
	return out
}

// Reset clears all recorded requests.
func (m *MockAirlock) Reset() {
	m.mu.Lock()
	m.requests = nil
	m.mu.Unlock()
}

// Close shuts down the mock server.
func (m *MockAirlock) Close() {
	m.Server.Close()
}

func (m *MockAirlock) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, MockRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   body,
	})
	m.mu.Unlock()
}
