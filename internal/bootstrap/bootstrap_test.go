package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDir(t *testing.T) {
	tests := []struct {
		name          string
		files         map[string]string
		wantBootstrap bool
		wantErr       bool
	}{
		{name: "empty"},
		{
			name: "bootstrap",
			files: map[string]string{
				"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n",
			},
			wantBootstrap: true,
		},
		{
			name: "bootstrap with sum",
			files: map[string]string{
				"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n",
				"go.sum": "sum\n",
			},
			wantBootstrap: true,
		},
		{
			name: "bootstrap with generated indirect requirements",
			files: map[string]string{
				"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire (\n github.com/airlockrun/agentsdk v0.4.0-rc.32 // indirect\n golang.org/x/mod v0.36.0 // indirect\n)\n\ntool github.com/airlockrun/agentsdk/cmd/air\n",
			},
			wantBootstrap: true,
		},
		{name: "wrong module", files: map[string]string{"go.mod": "module example.com/wrong\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n"}, wantErr: true},
		{name: "missing tool", files: map[string]string{"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n"}, wantErr: true},
		{name: "extra require", files: map[string]string{"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire (\n github.com/airlockrun/agentsdk v0.4.0-rc.32\n example.com/extra v1.0.0\n)\n\ntool github.com/airlockrun/agentsdk/cmd/air\n"}, wantErr: true},
		{name: "replace directive", files: map[string]string{"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\nreplace github.com/airlockrun/agentsdk => ../agentsdk\n\ntool github.com/airlockrun/agentsdk/cmd/air\n"}, wantErr: true},
		{name: "extra file", files: map[string]string{"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n", "notes.txt": "keep"}, wantErr: true},
		{name: "extra directory", files: map[string]string{"go.mod": "module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n", "nested/file": "keep"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tt.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := EnsureDir(dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EnsureDir error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantBootstrap {
				t.Fatalf("EnsureDir bootstrap = %v, want %v", got, tt.wantBootstrap)
			}
		})
	}
}

func TestEnsureDirCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	got, err := EnsureDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("missing directory reported as bootstrap")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestResetDirRemovesOnlyBootstrapFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module airlock.bootstrap\n\ngo 1.26\n\nrequire github.com/airlockrun/agentsdk v0.4.0-rc.32\n\ntool github.com/airlockrun/agentsdk/cmd/air\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("sum\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ResetDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ResetDir left entries: %v", entries)
	}
}

func TestInitializeDirWritesRetryableBootstrap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	if err := InitializeDir(dir, "v0.4.0-rc.32"); err != nil {
		t.Fatal(err)
	}
	if ok, err := EnsureDir(dir); err != nil || !ok {
		t.Fatalf("EnsureDir after InitializeDir = %v, %v", ok, err)
	}
	if err := InitializeDir(dir, "v0.4.0-rc.32"); err != nil {
		t.Fatalf("InitializeDir retry: %v", err)
	}
}
