package agentsdk

import (
	"net/http"
	"testing"
)

func testAgent(t *testing.T) (*Agent, *MockAirlock) {
	t.Helper()
	mock, url := NewMockAirlock()
	t.Cleanup(mock.Close)

	a := &Agent{
		agentID:      "test-agent",
		apiURL:       url,
		token:        "test-token",
		httpClient:   &http.Client{},
		sensitiveSet: make(map[string]struct{}),
		tools:        make(map[string]*registeredTool),
		webhooks:     make(map[string]webhookEntry),
		crons:        make(map[string]cronEntry),
		auths:        make(map[string]ConnectionDef),
		convVMConfig: DefaultConversationVMConfig(),
	}
	a.client = newAirlockClient(url, "test-token", a.httpClient)
	a.AddSensitive("test-token")
	return a, mock
}
