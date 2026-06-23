package agenttest

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/pressly/goose/v3"
)

// migrationsDir is where the scaffold keeps goose migrations.
const migrationsDir = "db/migrations"

// migrationFilePattern matches goose migration files (numeric prefix + name +
// .sql/.go), mirroring agentsdk's own check so UseDB skips goose entirely when
// an agent has no migrations yet (a fresh scaffold ships only doc.go).
var migrationFilePattern = regexp.MustCompile(`^\d+_.*\.(sql|go)$`)

// UseDB points the agent at the test database named by $TEST_DB_URL and brings
// its schema to a clean, fully-migrated state. It skips the test when
// $TEST_DB_URL is unset, so DB-backed tests run only where a test database is
// provisioned (the agent build environment sets it).
//
// It sets AIRLOCK_DB_URL (so agent.DB() connects there), then resets the schema
// — goose down to 0, then up — applying db/migrations. Go migrations run with
// the agent attached, exactly as in production. Call it after constructing the
// agent and before using agent.DB():
//
//	env := agenttest.NewEnv(t)
//	a := newAgent()
//	agenttest.UseDB(t, a)
//	// a.DB() now points at a freshly migrated test schema
func UseDB(t *testing.T, a *agentsdk.Agent) {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping DB-backed test")
	}
	t.Setenv("AIRLOCK_DB_URL", dsn)

	db := a.DB()
	if db == nil {
		t.Fatal("agenttest: agent.DB() is nil after setting AIRLOCK_DB_URL")
	}

	if !hasMigrationFiles(migrationsDir) {
		// No migrations to apply (e.g. a fresh scaffold) — the connection is
		// live and the schema is trivially up to date. goose errors on an
		// empty migration set, so don't call it.
		return
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("agenttest: goose dialect: %v", err)
	}
	ctx := a.MigrationContext(context.Background())
	if err := goose.DownToContext(ctx, db.Underlying(), migrationsDir, 0); err != nil {
		t.Fatalf("agenttest: reset schema (goose down): %v", err)
	}
	if err := goose.UpContext(ctx, db.Underlying(), migrationsDir); err != nil {
		t.Fatalf("agenttest: apply migrations (goose up): %v", err)
	}
}

// hasMigrationFiles reports whether dir contains at least one goose migration.
func hasMigrationFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && migrationFilePattern.MatchString(e.Name()) {
			return true
		}
	}
	return false
}
