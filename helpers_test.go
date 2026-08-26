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

	a := newAgentRegistrationState(Config{Description: "test agent"})
	a.agentID = "test-agent"
	a.apiURL = url
	a.token = "test-token"
	a.httpClient = &http.Client{}
	db, err := sql.Open("agentsdk-test", "")
	if err != nil {
		t.Fatal(err)
	}
	a.db.db = db
	t.Cleanup(func() { _ = db.Close() })
	a.client = newAirlockClient(url, "test-token", a.httpClient)
	a.AddSensitive("test-token")
	a.phase = agentRunning
	return a, mock
}
