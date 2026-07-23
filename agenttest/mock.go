package agenttest

import (
	"net/http/httptest"

	"github.com/airlockrun/agentsdk/internal/mockairlock"
)

// MockRequest records a request made to the mock Airlock server.
type MockRequest struct {
	Method string
	Path   string
	Body   []byte
}

// MockAirlock is an in-process Airlock API used by agent tests.
type MockAirlock struct {
	Server *httptest.Server

	// LLMResponse is the NDJSON response returned by the model endpoint.
	LLMResponse []byte

	mock *mockairlock.Mock
}

// NewMockAirlock creates a mock Airlock server and returns its base URL.
func NewMockAirlock() (*MockAirlock, string) {
	m := &MockAirlock{}
	inner, url := mockairlock.NewWithLLMResponse(func() []byte { return m.LLMResponse })
	m.Server = inner.Server
	m.mock = inner
	return m, url
}

// Requests returns all recorded requests.
func (m *MockAirlock) Requests() []MockRequest {
	return mockRequests(m.mock.Requests())
}

// RequestsByPath returns requests matching the path prefix.
func (m *MockAirlock) RequestsByPath(prefix string) []MockRequest {
	return mockRequests(m.mock.RequestsByPath(prefix))
}

// Reset clears all recorded requests.
func (m *MockAirlock) Reset() {
	m.mock.Reset()
}

// Close shuts down the mock server.
func (m *MockAirlock) Close() {
	m.mock.Close()
}

func mockRequests(requests []mockairlock.Request) []MockRequest {
	out := make([]MockRequest, len(requests))
	for i, request := range requests {
		out[i] = MockRequest{
			Method: request.Method,
			Path:   request.Path,
			Body:   request.Body,
		}
	}
	return out
}
