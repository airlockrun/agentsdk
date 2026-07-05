package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	if err := cmdInit([]string{dir, "--airlock", "https://airlock.example.com/"}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	for _, f := range []string{"go.mod", "AGENTS.md", "Dockerfile", "main.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected %s: %v", f, err)
		}
	}
	b, ok, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatalf("loadAgentBinding: %v", err)
	}
	if !ok || b.AirlockURL != "https://airlock.example.com" {
		t.Fatalf("binding = %#v, %v", b, ok)
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

func TestParseDeployFlags(t *testing.T) {
	f, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--url", "https://airlock.example.com", "repo"})
	if err != nil {
		t.Fatalf("parseDeployFlags: %v", err)
	}
	if !f.create || f.slug != "todo" || f.url != "https://airlock.example.com" || f.dir != "repo" {
		t.Fatalf("flags = %#v", f)
	}
	if _, err := parseDeployFlags([]string{"--create"}); err == nil {
		t.Fatal("--create without --slug returned nil error")
	}
	if _, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--agent", "todo"}); err == nil {
		t.Fatal("--create with --agent returned nil error")
	}
}

func TestResolveDeployTargetFailsOnBindingSlugMismatch(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/"+id {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agent":{"id":"` + id + `","slug":"real-slug"}}`))
	}))
	defer srv.Close()

	_, err := resolveDeployTarget(context.Background(), srv.URL, "tok", "", agentBinding{AgentID: id, Slug: "stale-slug"})
	if err == nil || !strings.Contains(err.Error(), "stale-slug") || !strings.Contains(err.Error(), "real-slug") {
		t.Fatalf("resolveDeployTarget error = %v", err)
	}
}

func TestResolveDeployTargetFailsOnBindingIDMismatch(t *testing.T) {
	const boundID = "11111111-1111-1111-1111-111111111111"
	const realID = "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agents":[{"id":"` + realID + `","slug":"todo"}]}`))
	}))
	defer srv.Close()

	_, err := resolveDeployTarget(context.Background(), srv.URL, "tok", "todo", agentBinding{AgentID: boundID, Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), boundID) || !strings.Contains(err.Error(), realID) {
		t.Fatalf("resolveDeployTarget error = %v", err)
	}
}

func TestResolveDeployTargetRejectsSlugOnlyBinding(t *testing.T) {
	_, err := resolveDeployTarget(context.Background(), "https://airlock.example.com", "tok", "", agentBinding{Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), "no agent_id") || !strings.Contains(err.Error(), "--agent todo") {
		t.Fatalf("resolveDeployTarget error = %v", err)
	}
}

func TestWriteSourceArchiveSkipsLocalState(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "secret")
	mustWrite(t, filepath.Join(dir, ".airlock", "agent.toml"), "slug = \"todo\"\n")
	mustWrite(t, filepath.Join(dir, ".airlock", "local", "storage", "uploads", "doc.txt"), "local")

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeSourceArchive(pw, dir)) }()
	gz, err := gzip.NewReader(pr)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		seen[h.Name] = true
	}
	if !seen["go.mod"] || !seen["main.go"] || !seen[".airlock/agent.toml"] {
		t.Fatalf("archive missing expected files: %#v", seen)
	}
	if seen[".git/config"] || seen[".airlock/local/storage/uploads/doc.txt"] {
		t.Fatalf("archive included local state: %#v", seen)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
