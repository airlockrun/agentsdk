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
		webhooks:     make(map[string]*Webhook),
		crons:        make(map[string]*Cron),
		auths:        make(map[string]*Connection),
		mcps:         make(map[string]*MCP),
		topics:       make(map[string]*Topic),
		routes:       make(map[string]*Route),
		convVMConfig: DefaultConversationVMConfig(),
	}
	// Auto-register the framework's /tmp directory the same way New does,
	// so tests have somewhere to read/write without setting it up by hand.
	a.directories = append(a.directories, &Directory{
		Path: reservedTmpPath, Read: AccessUser, Write: AccessUser, List: AccessUser,
	})
	a.client = newAirlockClient(url, "test-token", a.httpClient)
	a.AddSensitive("test-token")
	return a, mock
}
