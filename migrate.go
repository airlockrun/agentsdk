package agentsdk

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
)

const migrationsPath = "/migrations"

const (
	migrateMaxAttempts  = 8
	migrateRetryBackoff = 250 * time.Millisecond
)

// isTransientConnError reports whether err is a transient DB connection/auth
// error worth retrying at boot rather than panicking. Notably 28P01: Airlock
// reconciles an agent's Postgres role with ALTER ROLE ... PASSWORD, which
// rewrites the scram-sha-256 verifier (new salt) even for the *same* password.
// An ALTER that lands mid-handshake makes Postgres reject the correct password
// with 28P01; a retry once the ALTER completes succeeds. Connection-refused /
// "starting up" cover the agent racing Postgres coming up.
func isTransientConnError(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch string(pqErr.Code) {
		case "28P01", // invalid_password (verifier rewritten mid-handshake)
			"28000", // invalid_authorization_specification
			"57P03", // cannot_connect_now
			"53300", // too_many_connections
			"08006", // connection_failure
			"08001", // unable_to_establish_connection
			"08004": // rejected_connection
			return true
		default:
			return false
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "password authentication failed") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "the database system is starting up") ||
		strings.Contains(msg, "EOF")
}

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

// MigrationContext returns ctx with this agent attached, so goose Up/Down calls
// run the agent's Go migrations — those fetch the agent via
// AgentFromMigrationContext. autoMigrate uses this for the baked /migrations;
// tests use it to apply db/migrations against a test database (see
// agentsdk/agenttest.UseDB).
func (a *Agent) MigrationContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentCtxKey{}, a)
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
		// In validate or down-to mode we always exit — the agent shouldn't
		// continue to Serve() during these one-shot orchestrator invocations.
		if IsValidatingMigrations() {
			agentLogger().Info("no migrations to validate")
			os.Exit(0)
		}
		if os.Getenv("AGENT_MIGRATE_DOWN_TO") != "" {
			agentLogger().Info("no migrations to down-to")
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

	ctx := a.MigrationContext(context.Background())
	if IsValidatingMigrations() {
		agentLogger().Info("validating migrations (up → down → up)")
		if err := goose.UpContext(ctx, db, migrationsPath); err != nil {
			panic("agentsdk: validate up: " + err.Error())
		}
		if err := goose.DownToContext(ctx, db, migrationsPath, 0); err != nil {
			panic("agentsdk: validate down: " + err.Error())
		}
		if err := goose.UpContext(ctx, db, migrationsPath); err != nil {
			panic("agentsdk: validate re-up: " + err.Error())
		}
		agentLogger().Info("migrations validated successfully")
		os.Exit(0)
	}

	// One-shot down-to mode used by rollback. Airlock runs the agent's
	// current image with this env var set, against either a schema clone
	// (pre-flight check) or the live agent schema (the destructive step).
	// Exits 0 on success so the orchestrator can observe completion via
	// container exit code — same envelope as AGENT_VALIDATE_MIGRATIONS=1.
	if downStr := os.Getenv("AGENT_MIGRATE_DOWN_TO"); downStr != "" {
		v, err := strconv.ParseInt(downStr, 10, 64)
		if err != nil {
			panic("agentsdk: invalid AGENT_MIGRATE_DOWN_TO: " + err.Error())
		}
		agentLogger().Info("migrating down", zap.Int64("to_version", v))
		if err := goose.DownToContext(ctx, db, migrationsPath, v); err != nil {
			panic("agentsdk: down-to: " + err.Error())
		}
		agentLogger().Info("migrated down", zap.Int64("to_version", v))
		os.Exit(0)
	}

	// Retry on transient connection/auth errors: Airlock may be reconciling
	// this role's password (ALTER ROLE rewrites the scram verifier) and an
	// ALTER landing mid-handshake yields a spurious 28P01 for the correct
	// password. goose tracks the applied version, so re-running UpContext after
	// a connect failure is safe (no partial re-apply).
	var upErr error
	for attempt := 1; attempt <= migrateMaxAttempts; attempt++ {
		upErr = goose.UpContext(ctx, db, migrationsPath)
		if upErr == nil || !isTransientConnError(upErr) {
			break
		}
		backoff := migrateRetryBackoff * time.Duration(attempt)
		if backoff > time.Second {
			backoff = time.Second
		}
		agentLogger().Warn("migrations: transient DB connection error, retrying",
			zap.Int("attempt", attempt), zap.Int("max", migrateMaxAttempts), zap.Error(upErr))
		time.Sleep(backoff)
	}
	if upErr != nil {
		panic("agentsdk: run migrations: " + upErr.Error())
	}
	agentLogger().Info("migrations applied")
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
