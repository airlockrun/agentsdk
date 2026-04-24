package agentsdk

import (
	"context"
	"database/sql"
	"log"
	"os"
	"regexp"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

const migrationsPath = "/migrations"

// agentCtxKey is the context key under which the running Agent is stored
// during migrations. Go migrations retrieve the agent via AgentFromContext
// to perform storage/API calls.
type agentCtxKey struct{}

// AgentFromMigrationContext returns the Agent associated with a migration
// context. Panics if called outside of a migration — migrations receive the
// context via goose, which propagates it from autoMigrate.
func AgentFromMigrationContext(ctx context.Context) *Agent {
	a, ok := ctx.Value(agentCtxKey{}).(*Agent)
	if !ok {
		panic("agentsdk: AgentFromMigrationContext called outside a migration context")
	}
	return a
}

// IsValidatingMigrations reports whether the agent is running in migration
// validation mode (AGENT_VALIDATE_MIGRATIONS=1). Go migrations that touch
// external services (S3, Airlock API, connection credentials) should return
// early when this is true — those services are not available during
// build-time validation.
func IsValidatingMigrations() bool {
	return os.Getenv("AGENT_VALIDATE_MIGRATIONS") == "1"
}

// autoMigrate runs pending migrations from /migrations/ if the directory exists
// and a database is configured. Uses goose, which supports both .sql files and
// .go migrations registered via init(). Called automatically by New().
//
// Go migrations are picked up because main.go blank-imports the agent's
// db/migrations package, firing each file's init() before this function runs.
//
// If AGENT_VALIDATE_MIGRATIONS=1 is set, autoMigrate runs up → down → up to
// verify migrations are reversible, then calls os.Exit(0). Used by the Airlock
// build pipeline to validate migrations without booting the full agent. In
// validate mode, Go migrations that touch external services (S3, Airlock API,
// connection credentials) should skip their side effects — see doc.go example.
func (a *Agent) autoMigrate() {
	dsn := os.Getenv("AIRLOCK_DB_URL")
	if dsn == "" {
		return
	}
	if !hasMigrationFiles(migrationsPath) {
		// In validate mode we always exit — the agent shouldn't continue
		// to Serve() during build-time validation.
		if IsValidatingMigrations() {
			log.Println("agentsdk: no migrations to validate")
			os.Exit(0)
		}
		return
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		panic("agentsdk: open db for migrations: " + err.Error())
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		panic("agentsdk: goose dialect: " + err.Error())
	}

	ctx := context.WithValue(context.Background(), agentCtxKey{}, a)
	if IsValidatingMigrations() {
		log.Println("agentsdk: validating migrations (up → down → up)")
		if err := goose.UpContext(ctx, db, migrationsPath); err != nil {
			panic("agentsdk: validate up: " + err.Error())
		}
		if err := goose.DownToContext(ctx, db, migrationsPath, 0); err != nil {
			panic("agentsdk: validate down: " + err.Error())
		}
		if err := goose.UpContext(ctx, db, migrationsPath); err != nil {
			panic("agentsdk: validate re-up: " + err.Error())
		}
		log.Println("agentsdk: migrations validated successfully")
		os.Exit(0)
	}

	if err := goose.UpContext(ctx, db, migrationsPath); err != nil {
		panic("agentsdk: run migrations: " + err.Error())
	}
	log.Println("agentsdk: migrations applied")
}

// migrationFilePattern matches goose migration files: numeric prefix + name + .sql/.go.
// Excludes scaffold helpers like doc.go that share the package but aren't migrations.
var migrationFilePattern = regexp.MustCompile(`^\d+_.*\.(sql|go)$`)

// hasMigrationFiles checks if dir exists and contains at least one file
// matching goose's expected migration filename pattern.
func hasMigrationFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if migrationFilePattern.MatchString(e.Name()) {
			return true
		}
	}
	return false
}
