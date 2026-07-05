package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := newUUID()
		if err != nil {
			t.Fatalf("newUUID: %v", err)
		}
		if !uuidRe.MatchString(id) {
			t.Fatalf("uuid %q does not match v4 8-4-4-4-12 form", id)
		}
		if seen[id] {
			t.Fatalf("uuid %q generated twice", id)
		}
		seen[id] = true
	}
}

func TestTailwindAsset(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		{name: "linux amd64", goos: "linux", goarch: "amd64", want: "tailwindcss-linux-x64"},
		{name: "linux arm64", goos: "linux", goarch: "arm64", want: "tailwindcss-linux-arm64"},
		{name: "darwin amd64", goos: "darwin", goarch: "amd64", want: "tailwindcss-macos-x64"},
		{name: "darwin arm64", goos: "darwin", goarch: "arm64", want: "tailwindcss-macos-arm64"},
		{name: "unsupported os", goos: "windows", goarch: "amd64", wantErr: true},
		{name: "unsupported arch", goos: "linux", goarch: "386", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tailwindAsset(tt.goos, tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("tailwindAsset(%q, %q) = %q, want error", tt.goos, tt.goarch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("tailwindAsset(%q, %q): %v", tt.goos, tt.goarch, err)
			}
			if got != tt.want {
				t.Fatalf("tailwindAsset(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
			}
		})
	}
}

func TestEnsureEmptyDir(t *testing.T) {
	t.Run("creates missing dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "new")
		if err := ensureEmptyDir(dir); err != nil {
			t.Fatalf("ensureEmptyDir: %v", err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("dir not created: %v", err)
		}
	})

	t.Run("accepts empty existing dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := ensureEmptyDir(dir); err != nil {
			t.Fatalf("ensureEmptyDir on empty dir: %v", err)
		}
	})

	t.Run("rejects non-empty dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureEmptyDir(dir); err == nil {
			t.Fatal("ensureEmptyDir accepted a non-empty dir")
		}
	})
}

func TestCmdInitSmoke(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myagent")
	if err := cmdInit([]string{dir}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, f := range []string{"go.mod", "AGENTS.md", "Dockerfile", "main.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected %s: %v", f, err)
		}
	}
}

func TestCmdInitRejectsNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdInit([]string{dir}); err == nil {
		t.Fatal("cmdInit overwrote a non-empty dir")
	}
}

func TestCmdUpdateRequiresGoMod(t *testing.T) {
	t.Run("errors without go.mod", func(t *testing.T) {
		if err := cmdUpdate([]string{t.TempDir()}); err == nil {
			t.Fatal("cmdUpdate ran without a go.mod")
		}
	})

	t.Run("updates managed files", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "agent")
		if err := cmdInit([]string{dir}); err != nil {
			t.Fatalf("cmdInit: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		if err := cmdUpdate([]string{dir}); err != nil {
			t.Fatalf("cmdUpdate: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatalf("Dockerfile not updated: %v", err)
		}
	})
}
