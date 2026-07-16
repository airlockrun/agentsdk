package agentsdk

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestHasMigrationFiles(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		missing bool
		want    bool
	}{
		{name: "missing directory fails", missing: true},
		{name: "empty directory is valid"},
		{name: "SQL migration", files: []string{"00001_create.sql"}, want: true},
		{name: "Go migration", files: []string{"00002_seed.go"}, want: true},
		{name: "unrelated files", files: []string{"README.md", "migration.sql"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.missing {
				dir = filepath.Join(dir, "missing")
			}
			got, err := hasMigrationFiles(dir)
			if tt.missing {
				if err == nil {
					t.Fatal("hasMigrationFiles returned nil error for missing directory")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("hasMigrationFiles() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSourceMigrationsDir(t *testing.T) {
	t.Run("resolves from nested package directory", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/agent\n")
		nested := filepath.Join(root, "internal", "feature")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(nested)

		before, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		got, err := sourceMigrationsDir()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, "db", "migrations")
		if got != want {
			t.Fatalf("sourceMigrationsDir() = %q, want %q", got, want)
		}
		after, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("working directory changed from %q to %q", before, after)
		}
	})

	t.Run("uses nearest enclosing module", func(t *testing.T) {
		outer := t.TempDir()
		writeTestFile(t, filepath.Join(outer, "go.mod"), "module example.com/outer\n")
		inner := filepath.Join(outer, "tools", "agent")
		writeTestFile(t, filepath.Join(inner, "go.mod"), "module example.com/inner\n")
		nested := filepath.Join(inner, "pkg", "feature")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(nested)

		got, err := sourceMigrationsDir()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(inner, "db", "migrations")
		if got != want {
			t.Fatalf("sourceMigrationsDir() = %q, want %q", got, want)
		}
	})

	t.Run("fails without enclosing module", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		_, err := sourceMigrationsDir()
		if err == nil {
			t.Fatal("sourceMigrationsDir returned nil error")
		}
		if want := "find enclosing Go module from " + dir + ": go.mod not found"; err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}
	})

	t.Run("fails for invalid module marker", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "go.mod"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		_, err := sourceMigrationsDir()
		if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Fatalf("error = %v, want invalid module marker error", err)
		}
	})
}

func TestAutoMigrateSourceResolutionErrors(t *testing.T) {
	tests := []struct {
		name      string
		module    bool
		wantPanic string
	}{
		{name: "missing module", wantPanic: "agentsdk: resolve source migrations: find enclosing Go module"},
		{name: "missing migrations directory", module: true, wantPanic: "agentsdk: read migrations directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.module {
				writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/agent\n")
			}
			nested := filepath.Join(root, "pkg", "feature")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(nested)
			t.Setenv(testMigrationEnv, "1")
			defer func() {
				got := fmt.Sprint(recover())
				if got == "<nil>" {
					t.Fatal("autoMigrate did not panic")
				}
				if !strings.Contains(got, tt.wantPanic) {
					t.Fatalf("panic = %q, want substring %q", got, tt.wantPanic)
				}
			}()
			(&Agent{}).autoMigrate()
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationLockKeyIsStablePerAgent(t *testing.T) {
	first := migrationLockKey("agent-a")
	if first != migrationLockKey("agent-a") {
		t.Fatal("migration lock key changed for the same agent")
	}
	if first == migrationLockKey("agent-b") {
		t.Fatal("different agents received the same migration lock key")
	}
}

func TestWithMigrationLockSerializes(t *testing.T) {
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
		ctr, err := postgres.Run(context.Background(), "pgvector/pgvector:pg17",
			postgres.WithDatabase("agent_test"),
			postgres.WithUsername("agent"),
			postgres.WithPassword("agent"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start PostgreSQL container: %v", err)
		}
		t.Cleanup(func() {
			if err := ctr.Terminate(context.Background()); err != nil {
				t.Errorf("stop PostgreSQL container: %v", err)
			}
		})
		dsn, err = ctr.ConnectionString(context.Background(), "sslmode=disable")
		if err != nil {
			t.Fatalf("PostgreSQL connection string: %v", err)
		}
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := migrationLockKey("advisory-lock-test")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withMigrationLock(ctx, db, key, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withMigrationLock(ctx, db, key, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second migration entered while first held advisory lock")
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}
