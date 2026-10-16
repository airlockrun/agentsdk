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
			(&Agent{}).autoMigrate()
		})
	}
}
func TestMigrationLockKeyIsStablePerAgent(t *testing.T) {
	first := migrationLockKey("agent-a")
	if first != migrationLockKey("agent-a") || first == migrationLockKey("agent-b") {
		t.Fatal("migration lock identity is not stable/isolated")
	}
}
