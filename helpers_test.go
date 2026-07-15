package agentsdk

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"testing"

	"github.com/airlockrun/agentsdk/internal/mockairlock"
)

func init() {
	sql.Register("agentsdk-test", testDBDriver{})
}

type testDBDriver struct{}

func (testDBDriver) Open(string) (driver.Conn, error) { return testDBConn{}, nil }

type testDBConn struct{}

func (testDBConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (testDBConn) Close() error                        { return nil }
func (testDBConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }
func (testDBConn) Ping(context.Context) error          { return nil }

func testAgent(t *testing.T) (*Agent, *mockairlock.Mock) {
	t.Helper()
	mock, url := mockairlock.New()
	t.Cleanup(mock.Close)

	a := &Agent{
		agentID:          "test-agent",
		apiURL:           url,
		token:            "test-token",
		httpClient:       &http.Client{},
		sensitiveSet:     make(map[string]struct{}),
		tools:            make(map[string]*registeredTool),
		webhooks:         make(map[string]*Webhook),
		scheduleHandlers: make(map[string]*scheduleHandler),
		auths:            make(map[string]*Connection),
		mcps:             make(map[string]*MCP),
		topics:           make(map[string]*Topic),
		routes:           make(map[string]*Route),
		envVars:          make(map[string]*EnvVar),
		execEndpoints:    make(map[string]*ExecEndpoint),
		staticAssets:     make(map[string]*StaticAsset),
	}
	db, err := sql.Open("agentsdk-test", "")
	if err != nil {
		t.Fatal(err)
	}
	a.db = &AgentDB{db: db, agent: a}
	t.Cleanup(func() { _ = db.Close() })
	// Auto-register the framework's /tmp directory the same way New does,
	// so tests have somewhere to read/write without setting it up by hand.
	a.directories = append(a.directories, &Directory{
		Path: reservedTmpPath, Read: AccessUser, Write: AccessUser, List: AccessUser,
		Description: "Framework scratch directory",
	})
	a.client = newAirlockClient(url, "test-token", a.httpClient)
	a.AddSensitive("test-token")
	return a, mock
}
