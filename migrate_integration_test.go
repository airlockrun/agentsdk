//go:build airlockplatformintegration

package agentsdk

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// This is real PostgreSQL verification, explicitly selected by platform owners.
// It never provisions Docker or borrows the deployed app/platform database.
func TestWithMigrationLockSerializes(t *testing.T) {
	dsn := os.Getenv("AIRLOCK_PLATFORM_TEST_DB_URL")
	if dsn == "" {
		t.Fatal("explicit disposable AIRLOCK_PLATFORM_TEST_DB_URL is required")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	key := migrationLockKey(t.Name())
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withMigrationLock(ctx, database, key, func() error { close(firstEntered); <-releaseFirst; return nil })
	}()
	select {
	case <-firstEntered:
	case err := <-firstDone:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	secondEntered, secondDone := make(chan struct{}), make(chan error, 1)
	go func() {
		secondDone <- withMigrationLock(ctx, database, key, func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("concurrent migration entered")
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
