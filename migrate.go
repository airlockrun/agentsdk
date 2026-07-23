package agentsdk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
)

const (
	runtimeMigrationsPath  = "/migrations"
	sourceMigrationsPath   = "db/migrations"
	testMigrationEnv       = "AGENTSDK_TEST_MIGRATIONS"
	migrationTimeout       = 10 * time.Minute
	migrationUnlockTimeout = 5 * time.Second
)

// isTransientConnError reports whether err is a transient DB connection/auth
// error worth retrying before migrations start. A retry is safe here because
// no migration code or external side effect has run yet.
func isTransientConnError(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch string(pqErr.Code) {
		case "28P01", "28000", "57P03", "53300", "08006", "08001", "08004":
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

type agentCtxKey struct{}

func agentFromMigrationContext(ctx context.Context) *Agent {
	a, ok := ctx.Value(agentCtxKey{}).(*Agent)
	if !ok {
		panic("agentsdk: MigrationExternalStep called outside an SDK-managed migration")
	}
	return a
}

func (a *Agent) migrationContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentCtxKey{}, a)
}

func isValidatingMigrations() bool {
	return os.Getenv("AGENT_VALIDATE_MIGRATIONS") == "1"
}

type migrationMode int

const (
	migrationUp migrationMode = iota
	migrationValidate
	migrationDownTo
	migrationTestReset
)

// autoMigrate applies migrations from the runtime image. agenttest selects the
// canonical source directory and reset/up mode before constructing the agent.
func (a *Agent) autoMigrate() {
	dir := runtimeMigrationsPath
	mode := migrationUp
	var downTo int64

	if os.Getenv(testMigrationEnv) == "1" {
		var err error
		dir, err = sourceMigrationsDir()
		if err != nil {
			panic("agentsdk: resolve source migrations: " + err.Error())
		}
		mode = migrationTestReset
	} else if isValidatingMigrations() {
		mode = migrationValidate
	} else if downStr := os.Getenv("AGENT_MIGRATE_DOWN_TO"); downStr != "" {
		v, err := strconv.ParseInt(downStr, 10, 64)
		if err != nil {
			panic("agentsdk: invalid AGENT_MIGRATE_DOWN_TO: " + err.Error())
		}
		mode = migrationDownTo
		downTo = v
	}

	hasFiles, err := hasMigrationFiles(dir)
	if err != nil {
		panic("agentsdk: read migrations directory " + dir + ": " + err.Error())
	}
	if !hasFiles {
		agentLogger().Info("no migrations found", zap.String("directory", dir))
		if mode == migrationValidate || mode == migrationDownTo {
			os.Exit(0)
		}
		return
	}

	ctx, cancel := context.WithTimeout(a.migrationContext(context.Background()), migrationTimeout)
	defer cancel()
	if err := a.runMigrationPass(ctx, dir, mode, downTo); err != nil {
		panic("agentsdk: run migrations: " + err.Error())
	}

	switch mode {
	case migrationValidate:
		agentLogger().Info("migrations validated successfully")
		os.Exit(0)
	case migrationDownTo:
		agentLogger().Info("migrated down", zap.Int64("to_version", downTo))
		os.Exit(0)
	default:
		agentLogger().Info("migrations applied")
	}
}

func sourceMigrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	start := dir
	for {
		goMod := filepath.Join(dir, "go.mod")
		info, err := os.Stat(goMod)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("module marker %s is not a regular file", goMod)
			}
			return filepath.Join(dir, sourceMigrationsPath), nil
		case !os.IsNotExist(err):
			return "", fmt.Errorf("inspect module marker %s: %w", goMod, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("find enclosing Go module from %s: go.mod not found", start)
		}
		dir = parent
	}
}

func (a *Agent) runMigrationPass(ctx context.Context, dir string, mode migrationMode, downTo int64) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return withMigrationLock(ctx, a.db.db, migrationLockKey(a.agentID), func() error {
		switch mode {
		case migrationValidate:
			agentLogger().Info("validating migrations (up, down, up)")
			if err := goose.UpContext(ctx, a.db.db, dir); err != nil {
				return fmt.Errorf("validate up: %w", err)
			}
			if err := goose.DownToContext(ctx, a.db.db, dir, 0); err != nil {
				return fmt.Errorf("validate down: %w", err)
			}
			if err := goose.UpContext(ctx, a.db.db, dir); err != nil {
				return fmt.Errorf("validate re-up: %w", err)
			}
			return nil
		case migrationDownTo:
			agentLogger().Info("migrating down", zap.Int64("to_version", downTo))
			return goose.DownToContext(ctx, a.db.db, dir, downTo)
		case migrationTestReset:
			if err := goose.DownToContext(ctx, a.db.db, dir, 0); err != nil {
				return fmt.Errorf("test reset: %w", err)
			}
			return goose.UpContext(ctx, a.db.db, dir)
		default:
			return goose.UpContext(ctx, a.db.db, dir)
		}
	})
}

// migrationLockKey is stable across replicas of the same agent while allowing
// unrelated agents in the same PostgreSQL database to migrate independently.
func migrationLockKey(agentID string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("agentsdk:migrations:" + agentID))
	return int64(h.Sum64())
}

func withMigrationLock(ctx context.Context, db *sql.DB, key int64, fn func() error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), migrationUnlockTimeout)
		defer cancel()
		if _, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", key); err == nil && unlockErr != nil {
			err = unlockErr
		}
	}()
	return fn()
}

var migrationFilePattern = regexp.MustCompile(`^\d+_.*\.(sql|go)$`)

// hasMigrationFiles reports whether an existing directory contains a goose
// migration. Directory access errors are returned; an empty directory is valid.
func hasMigrationFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && migrationFilePattern.MatchString(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}
