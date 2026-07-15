package agenttest

import "github.com/airlockrun/agentsdk/internal/mockairlock"

// MockRequest records a request made to the mock Airlock server.
type MockRequest = mockairlock.Request

// MockAirlock is an in-process Airlock API used by agent tests.
type MockAirlock = mockairlock.Mock

// NewMockAirlock creates a mock Airlock server and returns its base URL.
func NewMockAirlock() (*MockAirlock, string) {
	return mockairlock.New()
}
