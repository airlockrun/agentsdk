package agentsdk

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestHasMigrationFiles(t *testing.T) {
	t.Run("missing directory fails", func(t *testing.T) {
		if _, err := hasMigrationFiles(t.TempDir() + "/missing"); err == nil {
			t.Fatal("hasMigrationFiles returned nil error for missing directory")
		}
	})

	t.Run("empty directory is valid", func(t *testing.T) {
		got, err := hasMigrationFiles(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Fatal("empty directory reported migration files")
		}
	})
}

func TestAutoMigrateFailsWhenSourceDirectoryIsMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(testMigrationEnv, "1")
	defer func() {
		got := fmt.Sprint(recover())
		if got == "<nil>" {
			t.Fatal("autoMigrate did not panic")
		}
		if want := "read migrations directory db/migrations"; !strings.Contains(got, want) {
			t.Fatalf("panic = %q, want substring %q", got, want)
		}
	}()
	(&Agent{}).autoMigrate()
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
