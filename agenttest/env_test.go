package agenttest_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
)

func TestNewSetsDatabaseAndMigratesBeforeFactoryContinues(t *testing.T) {
	workspace := t.TempDir()
	migrations := filepath.Join(workspace, "db", "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}
	migration := `-- +goose Up
CREATE TABLE bootstrap_order (id integer PRIMARY KEY);
-- +goose Down
DROP TABLE bootstrap_order;
`
	if err := os.WriteFile(filepath.Join(migrations, "00001_bootstrap.sql"), []byte(migration), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	t.Setenv("TEST_DB_URL", "")
	inherited := "postgres://production:secret@127.0.0.1:1/production"
	t.Setenv("AIRLOCK_DB_URL", inherited)

	var migratedTable string
	agenttest.New(t, func() *agentsdk.Agent {
		if os.Getenv("AIRLOCK_DB_URL") == inherited {
			t.Fatal("factory inherited AIRLOCK_DB_URL")
		}
		a := agentsdk.New(agentsdk.Config{Description: "test agent"})
		var table string
		if err := a.DB().QueryRowContext(t.Context(), `SELECT 'bootstrap_order'::regclass::text`).Scan(&table); err != nil {
			t.Fatalf("migration was not applied before factory continued: %v", err)
		}
		migratedTable = table
		rollback := errors.New("rollback")
		err := a.DB().Transaction(t.Context(), nil, func(tx agentsdk.DBTX) error {
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO bootstrap_order (id) VALUES (1)"); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("Transaction error = %v, want rollback sentinel", err)
		}
		var count int
		if err := a.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM bootstrap_order").Scan(&count); err != nil {
			t.Fatalf("count rolled-back rows: %v", err)
		}
		if count != 0 {
			t.Fatalf("rows after rollback = %d, want 0", count)
		}
		return a
	})

	if migratedTable != "bootstrap_order" {
		t.Fatalf("migrated table = %q, want bootstrap_order", migratedTable)
	}
}

func TestNewStaticAssetRoundTrip(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)

	env := agenttest.New(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "test agent"})
		a.RegisterStaticAsset(&agentsdk.StaticAsset{
			Name:        "app.css",
			ContentType: "text/css",
			Data:        []byte("body{}"),
		})
		return a
	})

	srv := httptest.NewServer(env.Agent.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/css" {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "body{}" {
		t.Errorf("body = %q, want %q", body, "body{}")
	}
}
