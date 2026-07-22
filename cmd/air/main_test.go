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

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/scaffold"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRunRejectsUnknownCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "multiple arguments", args: []string{"skill", "list"}, want: `unknown command "skill"; run 'air help' for usage`},
		{name: "directory without init", args: []string{"my-agent"}, want: `unknown command "my-agent"; run 'air help' for usage`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err == nil {
				t.Fatal("run() accepted an unknown command")
			}
			if err.Error() != tt.want {
				t.Fatalf("run() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestRunExplicitInitPreservesArgumentError(t *testing.T) {
	err := run([]string{"init", "one", "two"})
	if err == nil {
		t.Fatal("run() accepted init with two directories")
	}
	want := "init requires exactly one argument: the target directory"
	if err.Error() != want {
		t.Fatalf("run() error = %q, want %q", err, want)
	}
}

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
		{name: "windows amd64", goos: "windows", goarch: "amd64", want: "tailwindcss-windows-x64.exe"},
		{name: "windows arm64", goos: "windows", goarch: "arm64", wantErr: true},
		{name: "unsupported os", goos: "freebsd", goarch: "amd64", wantErr: true},
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

func TestSQLCAsset(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		{name: "linux amd64", goos: "linux", goarch: "amd64", want: "sqlc_1.30.0_linux_amd64.tar.gz"},
		{name: "darwin arm64", goos: "darwin", goarch: "arm64", want: "sqlc_1.30.0_darwin_arm64.tar.gz"},
		{name: "windows amd64", goos: "windows", goarch: "amd64", want: "sqlc_1.30.0_windows_amd64.zip"},
		{name: "windows arm64", goos: "windows", goarch: "arm64", wantErr: true},
		{name: "unsupported os", goos: "freebsd", goarch: "amd64", wantErr: true},
		{name: "unsupported arch", goos: "linux", goarch: "386", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sqlcAsset(tt.goos, tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sqlcAsset(%q, %q) = %q, want error", tt.goos, tt.goarch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sqlcAsset(%q, %q): %v", tt.goos, tt.goarch, err)
			}
			if got != tt.want {
				t.Fatalf("sqlcAsset(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
			}
		})
	}
}

func TestExtractSQLCBinary(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "sqlc.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	content := []byte("sqlc binary")
	if err := tw.WriteHeader(&tar.Header{Name: sqlcBinaryName(), Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "bin", sqlcBinaryName())
	if err := extractSQLCBinary(archivePath, dst); err != nil {
		t.Fatalf("extractSQLCBinary: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("extracted sqlc = %q, want %q", got, content)
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
	var tidied bool
	if err := runInit([]string{dir, "--airlock", "https://airlock.example.com/"}, func(dir string) error {
		tidied = true
		return os.WriteFile(filepath.Join(dir, "go.sum"), []byte("checksums\n"), 0o644)
	}); err != nil {
		t.Fatalf("cmdInit: %v", err)
	}
	if !tidied {
		t.Fatal("cmdInit did not tidy the initialized module")
	}
	for _, f := range []string{"go.mod", "go.sum", "AGENTS.md", "Dockerfile", "main.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("expected %s: %v", f, err)
		}
	}
	b, ok, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatalf("loadAgentBinding: %v", err)
	}
	remote, hasRemote := b.remote(defaultRemoteName)
	if !ok || !hasRemote || b.DefaultRemote != defaultRemoteName || remote.AirlockURL != "https://airlock.example.com" {
		t.Fatalf("binding = %#v, %v", b, ok)
	}
}

func TestAgentBindingRemoteSections(t *testing.T) {
	dir := t.TempDir()
	b := agentBinding{}
	b.putRemote("prod", agentRemoteBinding{AirlockURL: "https://airlock.example.com/", AgentID: "agent-1", Slug: "todo", SourceState: "sha256:prod"})
	b.putRemote("staging", agentRemoteBinding{AirlockURL: "https://staging.example.com", AgentID: "agent-2", Slug: "todo-staging"})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatalf("writeAgentBinding: %v", err)
	}
	got, ok, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatalf("loadAgentBinding: %v", err)
	}
	if !ok || got.DefaultRemote != "prod" {
		t.Fatalf("binding = %#v, ok=%v", got, ok)
	}
	prod, ok := got.remote("prod")
	if !ok || prod.AirlockURL != "https://airlock.example.com" || prod.AgentID != "agent-1" || prod.Slug != "todo" || prod.SourceState != "sha256:prod" {
		t.Fatalf("prod remote = %#v, ok=%v", prod, ok)
	}
	defaultRemote, ok := got.remote("")
	if !ok || defaultRemote.AgentID != "agent-1" {
		t.Fatalf("default remote = %#v, ok=%v", defaultRemote, ok)
	}
	staging, ok := got.remote("staging")
	if !ok || staging.AirlockURL != "https://staging.example.com" || staging.AgentID != "agent-2" || staging.Slug != "todo-staging" {
		t.Fatalf("staging remote = %#v, ok=%v", staging, ok)
	}
}

func TestAgentBindingExplicitDefault(t *testing.T) {
	dir := t.TempDir()
	b := agentBinding{}
	b.putRemote("prod", agentRemoteBinding{AirlockURL: "https://airlock.example.com", AgentID: "prod"})
	b.putRemote("dev", agentRemoteBinding{AirlockURL: "https://airlock.example.com", AgentID: "dev"})
	if err := b.setDefaultRemote("dev"); err != nil {
		t.Fatalf("setDefaultRemote: %v", err)
	}
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatalf("writeAgentBinding: %v", err)
	}
	got, _, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatalf("loadAgentBinding: %v", err)
	}
	if got.DefaultRemote != "dev" {
		t.Fatalf("DefaultRemote = %q, want dev", got.DefaultRemote)
	}
	if err := got.setDefaultRemote("missing"); err == nil {
		t.Fatal("setDefaultRemote accepted an undefined remote")
	}
}

func TestCmdRemoteDefault(t *testing.T) {
	dir := t.TempDir()
	b := agentBinding{}
	b.putRemote("prod", agentRemoteBinding{AirlockURL: "https://airlock.example.com", AgentID: "prod"})
	b.putRemote("dev", agentRemoteBinding{AirlockURL: "https://airlock.example.com", AgentID: "dev"})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := cmdRemote([]string{"default", "dev"}); err != nil {
		t.Fatalf("cmdRemote: %v", err)
	}
	got, _, err := loadAgentBinding(".")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultRemote != "dev" {
		t.Fatalf("DefaultRemote = %q, want dev", got.DefaultRemote)
	}
}

func TestCmdRemoteUnbindPreservesURLAndOtherRemotes(t *testing.T) {
	dir := t.TempDir()
	b := agentBinding{}
	b.putRemote("prod", agentRemoteBinding{
		AirlockURL:  "https://airlock.example.com",
		AgentID:     "11111111-1111-1111-1111-111111111111",
		Slug:        "todo",
		SourceState: "sha256:source",
	})
	b.putRemote("dev", agentRemoteBinding{
		AirlockURL: "https://dev.example.com",
		AgentID:    "22222222-2222-2222-2222-222222222222",
		Slug:       "todo-dev",
	})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := cmdRemote([]string{"unbind", "prod"}); err != nil {
		t.Fatalf("cmdRemote: %v", err)
	}

	got, _, err := loadAgentBinding(".")
	if err != nil {
		t.Fatal(err)
	}
	prod, ok := got.remote("prod")
	if !ok || prod.AirlockURL != "https://airlock.example.com" || prod.AgentID != "" || prod.Slug != "" || prod.SourceState != "" {
		t.Fatalf("prod remote = %#v, ok=%v", prod, ok)
	}
	dev, ok := got.remote("dev")
	if !ok || dev.AgentID != "22222222-2222-2222-2222-222222222222" || got.DefaultRemote != "prod" {
		t.Fatalf("binding = %#v", got)
	}
	if err := cmdRemote([]string{"unbind", "prod"}); err == nil || !strings.Contains(err.Error(), "not bound to an agent") {
		t.Fatalf("second unbind error = %v", err)
	}
}

func TestLoadAgentBindingRejectsDuplicateTOML(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level key",
			body: "default_remote = \"prod\"\ndefault_remote = \"dev\"\n[remotes.prod]\nurl = \"https://prod.example\"\n[remotes.dev]\nurl = \"https://dev.example\"\n",
		},
		{
			name: "remote section",
			body: "default_remote = \"prod\"\n[remotes.prod]\nurl = \"https://prod.example\"\n[remotes.prod]\nagent_id = \"agent\"\n",
		},
		{
			name: "remote key",
			body: "default_remote = \"prod\"\n[remotes.prod]\nagent_id = \"one\"\nagent_id = \"two\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, agentBindingPath), tt.body)
			if _, _, err := loadAgentBinding(dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("loadAgentBinding error = %v", err)
			}
		})
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
		if err := runInit([]string{dir}, func(string) error { return nil }); err != nil {
			t.Fatalf("cmdInit: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("custom-output/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var calls []string
		if err := runUpdateCommand(dir,
			func(string) error {
				calls = append(calls, "tidy")
				return nil
			},
			func(string, string) error {
				calls = append(calls, "toolchain")
				return nil
			},
		); err != nil {
			t.Fatalf("runUpdateCommand: %v", err)
		}
		if got := strings.Join(calls, ","); got != "tidy,toolchain" {
			t.Fatalf("update calls = %q, want tidy,toolchain", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatalf("Dockerfile not updated: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "custom-output/") {
			t.Fatal("cmdUpdate removed a user-owned ignore entry")
		}
		for _, pattern := range scaffold.GeneratedArtifactIgnorePatterns() {
			if !strings.Contains(string(body), pattern) {
				t.Errorf("cmdUpdate did not reconcile generated artifact pattern %q", pattern)
			}
		}
	})

	t.Run("rejects removed version flag", func(t *testing.T) {
		err := cmdUpdate([]string{"--agentsdk-version", "v9.9.9"})
		if err == nil || !strings.Contains(err.Error(), "unknown update flag") {
			t.Fatalf("cmdUpdate error = %v", err)
		}
	})
}

func TestReconcileAgentModule(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), `module agent

go 1.25.0

require (
	github.com/a-h/templ v0.2.0
	github.com/airlockrun/agentsdk v0.3.0
)
`)
	if err := reconcileAgentModule(dir, "v0.4.0-rc.30"); err != nil {
		t.Fatalf("reconcileAgentModule: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"go " + scaffold.GoVersion,
		"github.com/a-h/templ " + scaffold.TemplVersion,
		"github.com/airlockrun/agentsdk v0.4.0-rc.30",
		"github.com/a-h/templ/cmd/templ",
		"github.com/airlockrun/agentsdk/cmd/air",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("go.mod missing %q:\n%s", want, body)
		}
	}
}

func TestReconcileAgentModuleRequiresAgentSDK(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/not-an-agent\n\ngo 1.26.0\n")
	err := reconcileAgentModule(dir, "v0.4.0-rc.30")
	if err == nil || !strings.Contains(err.Error(), "no require directive for github.com/airlockrun/agentsdk") {
		t.Fatalf("reconcileAgentModule error = %v", err)
	}
}

func TestManagedScaffoldFlagsCannotBeOverridden(t *testing.T) {
	for _, flag := range []string{"--module", "--base-image"} {
		t.Run(strings.TrimPrefix(flag, "--"), func(t *testing.T) {
			if _, _, err := parseScaffoldFlags([]string{flag, "custom"}); err == nil {
				t.Fatalf("parseScaffoldFlags accepted %s", flag)
			}
		})
	}
}

func TestToolchainInstallRejectsPrefix(t *testing.T) {
	if err := cmdInstallToolchain([]string{"--prefix", t.TempDir()}); err == nil {
		t.Fatal("cmdInstallToolchain accepted --prefix")
	}
}

func TestUsageIncludesVersion(t *testing.T) {
	var out strings.Builder
	usage(&out)
	if !strings.Contains(out.String(), "air v"+agentsdk.Version) {
		t.Fatalf("usage does not identify CLI version:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "--reauthenticate") {
		t.Fatalf("usage does not document --reauthenticate:\n%s", out.String())
	}
}

func TestParseDeployFlags(t *testing.T) {
	f, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--url", "https://airlock.example.com", "--remote", "prod", "--message", "Add reminders", "repo"})
	if err != nil {
		t.Fatalf("parseDeployFlags: %v", err)
	}
	if !f.create || f.slug != "todo" || f.name != "todo" || f.url != "https://airlock.example.com" || f.remote != "prod" || f.message != "Add reminders" || f.dir != "repo" {
		t.Fatalf("flags = %#v", f)
	}
	f, err = parseDeployFlags([]string{"--create", "--name", "Sales Deck", "-m", "Initial implementation", "repo"})
	if err != nil {
		t.Fatalf("parseDeployFlags with derived slug: %v", err)
	}
	if f.slug != "sales-deck" || f.name != "Sales Deck" {
		t.Fatalf("derived flags = %#v", f)
	}
	if _, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--agent", "todo"}); err == nil {
		t.Fatal("--create with --agent returned nil error")
	}
	if _, err := parseDeployFlags([]string{"--slug", "todo", "-m", "Deploy"}); err == nil || !strings.Contains(err.Error(), "require --create") {
		t.Fatalf("create-only flag error = %v", err)
	}
	if _, err := parseDeployFlags([]string{"--remote", "bad remote"}); err == nil {
		t.Fatal("invalid --remote returned nil error")
	}
	if _, err := parseDeployFlags(nil); err == nil || !strings.Contains(err.Error(), "requires -m") {
		t.Fatalf("missing message error = %v", err)
	}
	f, err = parseDeployFlags([]string{"-m", "Fix retries"})
	if err != nil || f.message != "Fix retries" {
		t.Fatalf("short message flag = %#v, %v", f, err)
	}
	for _, message := range []string{"", " ", "first\nsecond", strings.Repeat("x", maxDeployMessageBytes+1)} {
		if _, err := parseDeployFlags([]string{"--message", message}); err == nil {
			t.Errorf("invalid message %q returned nil error", message)
		}
	}
}

func TestDeployCreateRejectsBoundRemoteWithRetryInstruction(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module agent\n")
	binding := agentBinding{}
	binding.putRemote("dev", agentRemoteBinding{
		AirlockURL: "https://airlock.example.com",
		AgentID:    "11111111-1111-1111-1111-111111111111",
		Slug:       "dev",
	})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	err := cmdDeploy([]string{dir, "--create", "--slug", "dev", "-m", "Initial implementation"})
	if err == nil || !strings.Contains(err.Error(), "without --create") {
		t.Fatalf("cmdDeploy error = %v", err)
	}
}

func TestSlugFromName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "Sales Deck", want: "sales-deck"},
		{name: " presentations_v2 ", want: "presentations-v2"},
		{name: "A", want: ""},
		{name: "--", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slugFromName(tt.name); got != tt.want {
				t.Fatalf("slugFromName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestEnsureDeploySDKVersion(t *testing.T) {
	t.Run("accepts matching version", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/agent-sdk" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Fatalf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"` + agentsdk.Version + `","commandImport":"github.com/airlockrun/agentsdk/cmd/air"}`))
		}))
		defer srv.Close()

		if err := ensureDeploySDKVersion(context.Background(), srv.URL, "tok"); err != nil {
			t.Fatalf("ensureDeploySDKVersion: %v", err)
		}
	})

	t.Run("accepts same pre-1.0 minor series", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"0.4.0-rc.18"}`))
		}))
		defer srv.Close()

		if err := ensureDeploySDKVersion(context.Background(), srv.URL, "tok"); err != nil {
			t.Fatalf("ensureDeploySDKVersion: %v", err)
		}
	})

	t.Run("rejects mismatched version", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"9.9.9","commandImport":"github.com/airlockrun/agentsdk/cmd/air"}`))
		}))
		defer srv.Close()

		err := ensureDeploySDKVersion(context.Background(), srv.URL, "tok")
		if err == nil {
			t.Fatal("ensureDeploySDKVersion accepted mismatched version")
		}
		for _, want := range []string{
			"Airlock uses agentsdk v9.9.9",
			"this air CLI is v" + agentsdk.Version,
			"go get -tool github.com/airlockrun/agentsdk/cmd/air@v9.9.9",
			"go tool air update",
			"go tool air build",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	})
}

func TestCompatibleSDKVersions(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "exact", a: "0.4.0-rc.19", b: "0.4.0-rc.19", want: true},
		{name: "leading v", a: "v0.4.0-rc.18", b: "0.4.0-rc.19", want: true},
		{name: "rc mismatch", a: "0.4.0-rc.18", b: "0.4.0-rc.19", want: true},
		{name: "newer patch", a: "0.4.0", b: "0.4.7", want: true},
		{name: "older patch", a: "0.4.7", b: "0.4.0", want: false},
		{name: "minor mismatch before v1", a: "0.4.9", b: "0.5.0", want: false},
		{name: "newer stable patch", a: "1.2.3", b: "1.2.9", want: true},
		{name: "stable minor mismatch", a: "1.2.3", b: "1.9.0", want: false},
		{name: "stable major mismatch", a: "1.9.0", b: "2.0.0", want: false},
		{name: "build metadata", a: "1.2.3+one", b: "1.2.3+two", want: true},
		{name: "invalid", a: "dev", b: "0.4.0", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compatibleSDKVersions(tt.a, tt.b); got != tt.want {
				t.Errorf("compatibleSDKVersions(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestEnsureToolchainProjectsCachedTools(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheDir, err := toolchainCacheDir()
	if err != nil {
		t.Fatalf("toolchainCacheDir: %v", err)
	}
	prefix := filepath.Join(t.TempDir(), ".airlock", "toolchain")
	moduleDir := t.TempDir()
	mustWrite(t, filepath.Join(moduleDir, "REFERENCE.md"), "# SDK reference\n")
	mustWrite(t, filepath.Join(moduleDir, "reference", "files.md"), "# Files\n")
	mustWrite(t, sqlcCachePath(cacheDir), "sqlc")
	mustWrite(t, tailwindCachePath(cacheDir), "tailwind")
	mustWrite(t, filepath.Join(daisyUICacheDir(cacheDir), "daisyui.mjs"), "daisyui")
	mustWrite(t, filepath.Join(daisyUICacheDir(cacheDir), "daisyui-theme.mjs"), "theme")

	if err := ensureToolchainFromModule(prefix, moduleDir); err != nil {
		t.Fatalf("ensureToolchainFromModule: %v", err)
	}
	if !toolchainComplete(prefix) {
		t.Fatal("toolchain is incomplete after projecting cached tools")
	}
	marker, err := os.ReadFile(filepath.Join(prefix, toolchainMarkerFile))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(marker) != toolchainMarker() {
		t.Fatalf("marker = %q, want %q", marker, toolchainMarker())
	}
	for _, tt := range []struct {
		path string
		want string
	}{
		{path: sqlcBinaryPath(prefix), want: "sqlc"},
		{path: tailwindBinaryPath(prefix), want: "tailwind"},
		{path: filepath.Join(prefix, "lib", "tailwind", "daisyui.mjs"), want: "daisyui"},
		{path: filepath.Join(prefix, "lib", "tailwind", "daisyui-theme.mjs"), want: "theme"},
	} {
		got, err := os.ReadFile(tt.path)
		if err != nil {
			t.Fatalf("read %s: %v", tt.path, err)
		}
		if string(got) != tt.want {
			t.Fatalf("%s = %q, want %q", tt.path, got, tt.want)
		}
	}
	for _, path := range []string{
		filepath.Join(prefix, "skills", "daisyui", "SKILL.md"),
		filepath.Join(prefix, "skills", "templ", "reference", "03-syntax-and-usage", "06-if-else.md"),
		filepath.Join(prefix, "skills", "htmx", "reference", "docs.md"),
		filepath.Join(prefix, "skills", "agentsdk", "SKILL.md"),
		filepath.Join(prefix, "skills", "agentsdk", "reference", "files.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("projected skill file %s: %v", path, err)
		}
	}
	mustWrite(t, filepath.Join(moduleDir, "REFERENCE.md"), "# Updated SDK reference\n")
	if err := ensureToolchainFromModule(prefix, moduleDir); err != nil {
		t.Fatalf("refresh agentsdk reference: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(prefix, "skills", "agentsdk", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "# Updated SDK reference") {
		t.Fatalf("agentsdk skill was not refreshed:\n%s", installed)
	}
}

func TestBuildStepsWritesBinaryOutsideRepository(t *testing.T) {
	output := filepath.Join(t.TempDir(), "agent")
	steps := buildSteps(".airlock/toolchain/bin/sqlc", ".airlock/toolchain/bin/tailwindcss", output, true)
	if len(steps) != 6 {
		t.Fatalf("len(buildSteps) = %d, want 6", len(steps))
	}
	wantNames := []string{"go mod tidy", "sqlc generate", "go tool templ generate", "tailwindcss", "go test -p=1 -count=1 ./...", "go build"}
	for i, want := range wantNames {
		if steps[i].name != want {
			t.Errorf("steps[%d].name = %q, want %q", i, steps[i].name, want)
		}
	}
	wantTest := []string{"go", "test", "-p=1", "-count=1", "./..."}
	gotTest := steps[len(steps)-2].cmd
	if strings.Join(gotTest, "\x00") != strings.Join(wantTest, "\x00") {
		t.Fatalf("test command = %q, want %q", gotTest, wantTest)
	}
	want := []string{"go", "build", "-buildvcs=false", "-o", output, "."}
	got := steps[len(steps)-1].cmd
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("build command = %q, want %q", got, want)
	}
}

func TestBuildStepsSkipsSQLCWithoutQueries(t *testing.T) {
	steps := buildSteps("sqlc", "tailwindcss", filepath.Join(t.TempDir(), "agent"), false)
	for _, step := range steps {
		if step.name == "sqlc generate" {
			t.Fatal("buildSteps included sqlc generation without query inputs")
		}
	}
}

func TestHasSQLQueries(t *testing.T) {
	dir := t.TempDir()
	if hasSQLQueries(dir) {
		t.Fatal("hasSQLQueries returned true without query files")
	}
	mustWrite(t, filepath.Join(dir, "db", "queries", "users.sql"), "-- name: ListUsers :many\nSELECT 1;\n")
	if !hasSQLQueries(dir) {
		t.Fatal("hasSQLQueries returned false with a query file")
	}
}

func TestCleanGeneratedDBFilesPreservesDocGo(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "internal", "db", "doc.go"), "package db\n")
	mustWrite(t, filepath.Join(dir, "internal", "db", "models.go"), "package db\n")
	if err := cleanGeneratedDBFiles(dir); err != nil {
		t.Fatalf("cleanGeneratedDBFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "db", "doc.go")); err != nil {
		t.Fatalf("doc.go was not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal", "db", "models.go")); !os.IsNotExist(err) {
		t.Fatalf("models.go was not removed: %v", err)
	}
}

func TestResolveAgentTargetRefreshesBindingSlug(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/"+id {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agent":{"id":"` + id + `","slug":"real-slug"}}`))
	}))
	defer srv.Close()

	target, err := resolveAgentTarget(context.Background(), srv.URL, "tok", "", "prod", agentRemoteBinding{AirlockURL: srv.URL, AgentID: id, Slug: "stale-slug", SourceState: "state"})
	if err != nil {
		t.Fatalf("resolveAgentTarget error = %v", err)
	}
	if target.AgentID != id || target.Slug != "real-slug" || target.SourceState != "state" {
		t.Fatalf("target = %#v", target)
	}
}

func TestResolveAgentTargetFailsOnBindingIDMismatch(t *testing.T) {
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

	_, err := resolveAgentTarget(context.Background(), srv.URL, "tok", "todo", "prod", agentRemoteBinding{AgentID: boundID, Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), boundID) || !strings.Contains(err.Error(), realID) || !strings.Contains(err.Error(), "different --remote") {
		t.Fatalf("resolveAgentTarget error = %v", err)
	}
}

func TestResolveAgentTargetDoesNotInheritUnboundSourceState(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"id":"` + id + `","slug":"dev"}]}`))
	}))
	defer srv.Close()

	target, err := resolveAgentTarget(context.Background(), srv.URL, "tok", "dev", "dev", agentRemoteBinding{
		AirlockURL: srv.URL, SourceState: "sha256:other-agent",
	})
	if err != nil {
		t.Fatalf("resolveAgentTarget: %v", err)
	}
	if target.AgentID != id || target.SourceState != "" {
		t.Fatalf("target = %#v", target)
	}
}

func TestResolveAgentTargetRejectsWrongUUIDResponse(t *testing.T) {
	const requestedID = "11111111-1111-1111-1111-111111111111"
	const returnedID = "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent":{"id":"` + returnedID + `","slug":"other"}}`))
	}))
	defer srv.Close()

	_, err := resolveAgentTarget(context.Background(), srv.URL, "tok", requestedID, "dev", agentRemoteBinding{})
	if err == nil || !strings.Contains(err.Error(), returnedID) {
		t.Fatalf("resolveAgentTarget error = %v", err)
	}
}

func TestResolveAgentTargetRequiresAgent(t *testing.T) {
	_, err := resolveAgentTarget(context.Background(), "https://airlock.example.com", "tok", "", "prod", agentRemoteBinding{Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), "remote \"prod\" needs an agent target") {
		t.Fatalf("resolveAgentTarget error = %v", err)
	}
}

func TestWriteSourceArchiveSkipsLocalState(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "secret")
	mustWrite(t, filepath.Join(dir, ".airlock", "local", "agent.toml"), "slug = \"todo\"\n")
	mustWrite(t, filepath.Join(dir, ".airlock", "local", "storage", "uploads", "doc.txt"), "local")
	mustWrite(t, filepath.Join(dir, ".airlock", "toolchain", "bin", "tailwindcss"), "binary")

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
	if !seen["go.mod"] || !seen["main.go"] {
		t.Fatalf("archive missing expected files: %#v", seen)
	}
	if seen[".git/config"] || seen[".airlock/local/agent.toml"] || seen[".airlock/local/storage/uploads/doc.txt"] || seen[".airlock/toolchain/bin/tailwindcss"] {
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
