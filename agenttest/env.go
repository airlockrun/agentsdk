// Package agenttest provides helpers for testing agents built on agentsdk.
// It lives in its own package so the testing dependency it pulls in never
// reaches an agent's production binary — agents import it only from _test.go.
package agenttest

import (
	"context"
	"os"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const postgresImage = "pgvector/pgvector:pg17"

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
// When Airlock provisions a test database in TEST_DB_URL, NewEnv also sets
// AIRLOCK_DB_URL before the caller builds the agent. DB-backed tests should use
// NewDBEnv, which additionally provisions a local database when needed.
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

// NewDBEnv starts a test environment with a PostgreSQL database. It uses the
// database supplied by Airlock in TEST_DB_URL when available. Otherwise it
// starts a throwaway pgvector container and skips the test when Docker is not
// available. Call it before constructing the agent so dependencies may cache
// agent.DB() safely.
func NewDBEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t)
	if os.Getenv("TEST_DB_URL") != "" {
		return env
	}

	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("agent_test"),
		postgres.WithUsername("agent"),
		postgres.WithPassword("agent"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("agenttest: start PostgreSQL container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Errorf("agenttest: stop PostgreSQL container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("agenttest: PostgreSQL connection string: %v", err)
	}
	t.Setenv("TEST_DB_URL", dsn)
	t.Setenv("AIRLOCK_DB_URL", dsn)
	return env
}
