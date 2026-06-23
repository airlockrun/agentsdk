// Package agenttest provides helpers for testing agents built on agentsdk.
// It lives in its own package so the testing dependency it pulls in never
// reaches an agent's production binary — agents import it only from _test.go.
package agenttest

import (
	"os"
	"testing"

	"github.com/airlockrun/agentsdk"
)

// Env is a test environment for an agent: a mock Airlock server plus the
// AIRLOCK_* environment variables wired to point at it.
type Env struct {
	// Airlock is the mock Airlock server. Inspect Airlock.Requests() to
	// assert on the calls a handler made.
	Airlock *agentsdk.MockAirlock
	// URL is the mock Airlock's base URL (also set as AIRLOCK_API_URL).
	URL string
}

// NewEnv starts a mock Airlock and sets the environment variables agentsdk.New
// requires (AIRLOCK_API_URL, AIRLOCK_AGENT_ID, AIRLOCK_AGENT_TOKEN) to point at
// it. Call it before constructing the agent. The mock server and the env vars
// are torn down automatically when the test ends.
//
// When a test database is provisioned ($TEST_DB_URL), NewEnv also sets
// AIRLOCK_DB_URL to it — up front, before the caller builds the agent — so an
// agent that caches agent.DB() in its Deps at construction (the recommended
// pattern) gets a live handle, exactly as in production. agenttest.UseDB then
// resets and migrates that schema. Without $TEST_DB_URL the var stays unset and
// UseDB skips the test.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	m, url := agentsdk.NewMockAirlock()
	t.Cleanup(m.Close)
	t.Setenv("AIRLOCK_API_URL", url)
	t.Setenv("AIRLOCK_AGENT_ID", "00000000-0000-0000-0000-000000000000")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "test-token")
	if dsn := os.Getenv("TEST_DB_URL"); dsn != "" {
		t.Setenv("AIRLOCK_DB_URL", dsn)
	}
	return &Env{Airlock: m, URL: url}
}
