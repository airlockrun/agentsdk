package buildconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndStage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "airlock.toml", "[build]\nruntime_files = [\"python-app/\", \"templates/page.html\"]\n", 0o644)
	writeFile(t, root, ".gitignore", "python-app/secret.env\n", 0o644)
	writeFile(t, root, "python-app/main.py", "print('ok')\n", 0o755)
	writeFile(t, root, "python-app/secret.env", "SECRET=bad\n", 0o600)
	writeFile(t, root, "templates/page.html", "ok\n", 0o644)

	selection, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Files) != 2 {
		t.Fatalf("selected files = %#v", selection.Files)
	}
	dst := filepath.Join(t.TempDir(), "runtime")
	if err := Stage(root, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "python-app", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("main.py mode = %o, want 755", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dst, "python-app", "secret.env")); !os.IsNotExist(err) {
		t.Fatalf("ignored secret was staged: %v", err)
	}
}

func TestLoadOptionalAndEmpty(t *testing.T) {
	root := t.TempDir()
	if selection, err := Load(root); err != nil || len(selection.Paths) != 0 {
		t.Fatalf("missing manifest: selection=%#v err=%v", selection, err)
	}
	writeFile(t, root, "airlock.toml", "[build]\nruntime_files = []\n", 0o644)
	if selection, err := Load(root); err != nil || len(selection.Paths) != 0 {
		t.Fatalf("empty manifest: selection=%#v err=%v", selection, err)
	}
}

func TestLoadRejectsInvalidManifest(t *testing.T) {
	for _, tt := range []struct {
		name     string
		manifest string
		prepare  func(string)
		want     string
	}{
		{name: "unknown key", manifest: "[build]\nruntime_files = []\nport = 8080\n", want: "unknown keys: build.port"},
		{name: "absolute", manifest: "[build]\nruntime_files = [\"/etc/passwd\"]\n", want: "must be relative"},
		{name: "traversal", manifest: "[build]\nruntime_files = [\"../secret\"]\n", want: "stay within"},
		{name: "glob", manifest: "[build]\nruntime_files = [\"static/*\"]\n", want: "globs are not supported"},
		{name: "missing", manifest: "[build]\nruntime_files = [\"missing.txt\"]\n", want: "does not exist"},
		{name: "ignored directory", manifest: "[build]\nruntime_files = [\"private/\"]\n", prepare: func(root string) {
			writeFile(t, root, ".gitignore", "private/\n", 0o644)
			writeFile(t, root, "private/secret", "bad", 0o600)
		}, want: "excluded from the source bundle"},
		{name: "duplicate", manifest: "[build]\nruntime_files = [\"static/\", \"static\"]\n", prepare: func(root string) { mustMkdir(t, root, "static") }, want: "overlap"},
		{name: "overlap", manifest: "[build]\nruntime_files = [\"static/\", \"static/app.js\"]\n", prepare: func(root string) { writeFile(t, root, "static/app.js", "x", 0o644) }, want: "overlap"},
		{name: "symlink", manifest: "[build]\nruntime_files = [\"app\"]\n", prepare: func(root string) {
			if err := os.Symlink("outside", filepath.Join(root, "app")); err != nil {
				t.Fatal(err)
			}
		}, want: "special file app"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "airlock.toml", tt.manifest, 0o644)
			if tt.prepare != nil {
				tt.prepare(root)
			}
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsSymlinkEscapeWithinDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "airlock.toml", "[build]\nruntime_files = [\"app/\"]\n", 0o644)
	mustMkdir(t, root, "app")
	if err := os.Symlink("../../secret", filepath.Join(root, "app", "secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "special file app/secret") {
		t.Fatalf("Load() error = %v", err)
	}
}

func writeFile(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}
