package mockairlock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/airlockrun/sol/websearch"
)

// Request records a request made to the mock Airlock server.
type Request struct {
	Method string
	Path   string
	Body   []byte
}

// Mock is an httptest server that implements the Airlock agent API.
type Mock struct {
	Server   *httptest.Server
	mu       sync.Mutex
	requests []Request

	// LLMResponse is the NDJSON response returned by the model endpoint.
	LLMResponse []byte
	// BeforeLLMResponse, when set, runs after the request is recorded and before
	// response headers or events are written.
	BeforeLLMResponse func()
}

// New creates a mock Airlock server and returns it with its base URL.
func New() (*Mock, string) {
	return NewWithLLMResponse(nil)
}

// NewWithLLMResponse creates a mock whose model response is supplied per call.
func NewWithLLMResponse(response func() []byte) (*Mock, string) {
	m := &Mock{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/agent/proxy/{slug}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("PUT /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		_, _ = w.Write([]byte("mock-file-content"))
	})
	mux.HandleFunc("DELETE /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/agent/storage", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		_ = json.NewEncoder(w).Encode([]any{})
	})
	mux.HandleFunc("POST /api/agent/storage/copy", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/agent/storage/info", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": "tmp/test.txt", "filename": "test.txt", "size": 42, "contentType": "text/plain",
		})
	})

	mux.HandleFunc("POST /api/agent/print", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/agent/topic/{slug}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/agent/topic/{slug}/subscribe", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/agent/llm/stream", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		if m.BeforeLLMResponse != nil {
			m.BeforeLLMResponse()
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		llmResponse := m.LLMResponse
		if response != nil {
			llmResponse = response()
		}
		if llmResponse != nil {
			_, _ = w.Write(llmResponse)
			return
		}
		_, _ = w.Write([]byte("{\"type\":\"start\",\"data\":{}}\n"))
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"data\":{\"text\":\"Hello\"}}\n"))
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"data\":{\"finishReason\":\"stop\",\"usage\":{\"inputTokens\":{\"total\":10},\"outputTokens\":{\"total\":5}}}}\n"))
	})
	mux.HandleFunc("POST /api/agent/llm/image", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"images":   []map[string]string{{"base64": "bW9jay1pbWFnZS1kYXRh", "mimeType": "image/png"}},
			"warnings": []string{},
		})
	})
	mux.HandleFunc("POST /api/agent/llm/embedding", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{{"values": []float64{0.1, 0.2, 0.3}, "index": 0}},
			"usage":      map[string]int{"tokens": 5},
		})
	})
	mux.HandleFunc("POST /api/agent/llm/speech", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"audio": "bW9jay1hdWRpbw==", "mimeType": "audio/mpeg"})
	})
	mux.HandleFunc("POST /api/agent/llm/transcription", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "mock transcription", "language": "en"})
	})
	mux.HandleFunc("POST /api/agent/search", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(websearch.Response{
			Results:  []websearch.Result{{Title: "Mock Result", URL: "https://example.com", Snippet: "mock"}},
			Provider: "mock",
		})
	})

	mux.HandleFunc("POST /api/agent/run/create", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": "run-mock-123"})
	})
	mux.HandleFunc("POST /api/agent/run/complete", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/agent/schedules", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/agent/connections/{slug}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /api/agent/sync", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"promptData": map[string]string{"agentRouteUrl": "https://mock-agent.test"},
		})
	})
	mux.HandleFunc("POST /api/agent/upgrade", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusAccepted)
	})

	m.Server = httptest.NewServer(mux)
	return m, m.Server.URL
}

// Requests returns all recorded requests.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// RequestsByPath returns requests matching the path prefix.
func (m *Mock) RequestsByPath(prefix string) []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Request
	for _, r := range m.requests {
		if len(r.Path) >= len(prefix) && r.Path[:len(prefix)] == prefix {
			out = append(out, r)
		}
	}
	return out
}

// Reset clears all recorded requests.
func (m *Mock) Reset() {
	m.mu.Lock()
	m.requests = nil
	m.mu.Unlock()
}

// Close shuts down the mock server.
func (m *Mock) Close() {
	m.Server.Close()
}

func (m *Mock) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body})
	m.mu.Unlock()
}
