package agentsdk

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasMigrationFiles(t *testing.T) {
	for _, tt := range []struct {
		name          string
		files         []string
		missing, want bool
	}{
		{name: "missing directory fails", missing: true}, {name: "empty directory is valid"},
		{name: "SQL migration", files: []string{"00001_create.sql"}, want: true},
		{name: "Go migration", files: []string{"00002_seed.go"}, want: true},
		{name: "unrelated files", files: []string{"README.md", "migration.sql"}},
	} {
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
					t.Fatal("missing directory accepted")
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("files = %t, %v; want %t", got, err, tt.want)
			}
		})
	}
}
func TestAutoMigrateRequiresExplicitRuntimeDirectory(t *testing.T) {
	for _, path := range []string{"relative/migrations", filepath.Join(t.TempDir(), "missing")} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("AIRLOCK_MIGRATIONS_DIR", path)
			defer func() {
				got := fmt.Sprint(recover())
				if got == "<nil>" || (!strings.Contains(got, "must be absolute") && !strings.Contains(got, "read migrations directory")) {
					t.Fatalf("panic = %s", got)
				}
			}()
			(&Agent{}).autoMigrate(migrationUp, 0)
		})
	}
}
func TestMigrationLockKeyIsStablePerAgent(t *testing.T) {
	first := migrationLockKey("agent-a")
	if first != migrationLockKey("agent-a") || first == migrationLockKey("agent-b") {
		t.Fatal("migration lock identity is not stable/isolated")
	}
}

func TestConfiguredMigrationMode(t *testing.T) {
	for _, tt := range []struct {
		name, validate, down, up string
		mode                     migrationMode
		version                  int64
		invalid                  bool
	}{
		{name: "normal runtime", mode: migrationUp},
		{name: "validation", validate: "1", mode: migrationValidate},
		{name: "down to zero", down: "0", mode: migrationDownTo},
		{name: "down to version", down: "7", mode: migrationDownTo, version: 7},
		{name: "up only", up: "1", mode: migrationUpOnly},
		{name: "unrecognized validation flag remains normal", validate: "true", mode: migrationUp},
		{name: "unrecognized up flag remains normal", up: "true", mode: migrationUp},
		{name: "invalid down fails", down: "invalid", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_VALIDATE_MIGRATIONS", tt.validate)
			t.Setenv("AGENT_MIGRATE_DOWN_TO", tt.down)
			t.Setenv("AGENT_MIGRATE_UP_ONLY", tt.up)
			mode, version, err := configuredMigrationMode()
			if (err != nil) != tt.invalid || !tt.invalid && (mode != tt.mode || version != tt.version) {
				t.Fatalf("mode=%v version=%d err=%v", mode, version, err)
			}
		})
	}
}
