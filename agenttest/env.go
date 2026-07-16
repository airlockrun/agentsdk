// Package agenttest provides agent environments, platform mocks, and caller
// contexts for tests of agents built on agentsdk.
package agenttest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const postgresImage = "pgvector/pgvector:pg17"

// Env is a fully constructed test agent with its mock Airlock server.
type Env struct {
	Agent *agentsdk.Agent
	// Airlock records calls made through the platform API.
	Airlock *MockAirlock
	// URL is the mock Airlock base URL.
	URL string
}

// New configures a mock Airlock and test database before invoking factory.
// TEST_DB_URL is used when explicitly supplied; otherwise New starts a
// throwaway pgvector container. agentsdk.New finds db/migrations from the
// enclosing Go module, then resets and applies them before it returns to the
// rest of factory, so dependency construction and registrations always see a
// clean, migrated schema.
func New(t *testing.T, factory func() *agentsdk.Agent) *Env {
	t.Helper()
	if factory == nil {
		t.Fatal("agenttest: factory is required")
	}

	mock, url := NewMockAirlock()
	t.Cleanup(mock.Close)
	t.Setenv("AIRLOCK_API_URL", url)
	t.Setenv("AIRLOCK_AGENT_ID", "00000000-0000-0000-0000-000000000000")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "test-token")
	t.Setenv("AGENT_VALIDATE_MIGRATIONS", "")
	t.Setenv("AGENT_MIGRATE_DOWN_TO", "")
	t.Setenv("AGENTSDK_TEST_MIGRATIONS", "1")

	// AIRLOCK_DB_URL is production input and may be inherited from the shell.
	// Tests only opt into an existing database through TEST_DB_URL.
	t.Setenv("AIRLOCK_DB_URL", "")
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := ctr.Terminate(ctx); err != nil {
				t.Errorf("agenttest: stop PostgreSQL container: %v", err)
			}
		})
		dsn, err = ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("agenttest: PostgreSQL connection string: %v", err)
		}
	}
	t.Setenv("AIRLOCK_DB_URL", dsn)

	a := factory()
	if a == nil {
		t.Fatal("agenttest: factory returned nil")
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("agenttest: close agent database: %v", err)
		}
	})
	return &Env{Agent: a, Airlock: mock, URL: url}
}
