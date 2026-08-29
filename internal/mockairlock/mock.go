package mockairlock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/websearch"
)

// Request records a request made to the mock Airlock server.
type Request struct {
	Method string
	Path   string
	Body   []byte
	Header http.Header
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
	// EnqueueJobResponse and GetJobResponse override the default job responses.
	EnqueueJobResponse        *wire.EnqueueJobResponse
	EnqueueJobError           *wire.EnqueueJobErrorResponse
	GetJobResponse            *wire.GetJobResponse
	RunCompleteStatus         int
	JobProgressStatus         int
	ConnectorCommandResponses map[string]json.RawMessage
	ConnectorJobResponses     map[string]json.RawMessage
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
		if m.RunCompleteStatus != 0 {
			http.Error(w, "run completion rejected", m.RunCompleteStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/agent/jobs", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		if m.EnqueueJobError != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(m.EnqueueJobError)
			return
		}
		response := m.EnqueueJobResponse
		if response == nil {
			var request wire.EnqueueJobRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			now := time.Now().UTC()
			response = &wire.EnqueueJobResponse{
				Job: wire.JobInfo{
					ID: request.ID, AgentID: "test-agent", HandlerName: request.Name, HandlerVersion: request.Version,
					InputSchemaHash: request.InputSchemaHash, OutputSchemaHash: request.OutputSchemaHash,
					Status: "queued", Input: request.Input, MaxAttempts: 3, AttemptLimit: 100, SourceRunID: r.Header.Get("X-Airlock-Run-ID"),
					ScheduledAt: request.ScheduledAt,
					CreatedAt:   now, UpdatedAt: now,
				},
				Created: true,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("GET /api/agent/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		if m.GetJobResponse == nil {
			http.Error(w, "GetJobResponse is not configured", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.GetJobResponse)
	})
	mux.HandleFunc("DELETE /api/agent/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/agent/jobs/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		if m.JobProgressStatus != 0 {
			http.Error(w, "job progress rejected", m.JobProgressStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/agent/connections/{slug}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /api/agent/env-vars/{slug}", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /api/agent/connectors/{need}/commands/{command}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		var request protocol.CommandCallRequest
		if err := strictJSON(r.Body, &request); err != nil || request.Mode != protocol.CommandModeUnary || request.Revision < 1 || request.InputSchemaHash == "" || request.OutputSchemaHash == "" || !json.Valid(request.Input) {
			http.Error(w, "invalid connector command request", http.StatusBadRequest)
			return
		}
		output, ok := m.ConnectorCommandResponses[r.PathValue("command")]
		if !ok {
			http.Error(w, "connector command response is not configured", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(wireResponse("mock-connector-job", output))
	})
	mux.HandleFunc("POST /api/agent/connectors/{need}/jobs/{command}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		var request protocol.CommandCallRequest
		if err := strictJSON(r.Body, &request); err != nil || request.Mode != protocol.CommandModeJob || request.Revision < 1 || request.InputSchemaHash == "" || request.OutputSchemaHash == "" || !json.Valid(request.Input) {
			http.Error(w, "invalid connector job request", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if m.ConnectorJobResponses == nil {
			m.ConnectorJobResponses = make(map[string]json.RawMessage)
		}
		m.ConnectorJobResponses["mock-connector-job"] = append(json.RawMessage(nil), m.ConnectorCommandResponses[r.PathValue("command")]...)
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"jobId": "mock-connector-job", "status": "queued"})
	})
	mux.HandleFunc("GET /api/agent/connectors/{need}/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		output := append(json.RawMessage(nil), m.ConnectorJobResponses[r.PathValue("job")]...)
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jobId": r.PathValue("job"), "status": "success", "output": output})
	})
	mux.HandleFunc("DELETE /api/agent/connectors/{need}/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		w.WriteHeader(http.StatusNoContent)
	})

	m.Server = httptest.NewServer(mux)
	return m, m.Server.URL
}

func wireResponse(jobID string, output json.RawMessage) map[string]any {
	return map[string]any{"jobId": jobID, "status": "success", "output": output}
}

func strictJSON(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func (m *Mock) SetConnectorCommandResponse(name string, output json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ConnectorCommandResponses == nil {
		m.ConnectorCommandResponses = make(map[string]json.RawMessage)
	}
	m.ConnectorCommandResponses[name] = append(json.RawMessage(nil), output...)
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
	r.Body = io.NopCloser(bytes.NewReader(body))
	m.mu.Lock()
	m.requests = append(m.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body, Header: r.Header.Clone()})
	m.mu.Unlock()
}
