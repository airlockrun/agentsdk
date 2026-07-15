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
	"time"

	"github.com/airlockrun/agentsdk"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
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
	b.setRemote("prod", agentRemoteBinding{AirlockURL: "https://airlock.example.com/", AgentID: "agent-1", Slug: "todo", SourceState: "sha256:prod"})
	b.setRemote("staging", agentRemoteBinding{AirlockURL: "https://staging.example.com", AgentID: "agent-2", Slug: "todo-staging"})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatalf("writeAgentBinding: %v", err)
	}
	got, ok, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatalf("loadAgentBinding: %v", err)
	}
	if !ok || got.DefaultRemote != "staging" {
		t.Fatalf("binding = %#v, ok=%v", got, ok)
	}
	prod, ok := got.remote("prod")
	if !ok || prod.AirlockURL != "https://airlock.example.com" || prod.AgentID != "agent-1" || prod.Slug != "todo" || prod.SourceState != "sha256:prod" {
		t.Fatalf("prod remote = %#v, ok=%v", prod, ok)
	}
	staging, ok := got.remote("")
	if !ok || staging.AirlockURL != "https://staging.example.com" || staging.AgentID != "agent-2" || staging.Slug != "todo-staging" {
		t.Fatalf("default remote = %#v, ok=%v", staging, ok)
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
		if err := cmdUpdate([]string{dir}); err != nil {
			t.Fatalf("cmdUpdate: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatalf("Dockerfile not updated: %v", err)
		}
	})
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
}

func TestParseDeployFlags(t *testing.T) {
	f, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--url", "https://airlock.example.com", "--remote", "prod", "--message", "Add reminders", "repo"})
	if err != nil {
		t.Fatalf("parseDeployFlags: %v", err)
	}
	if !f.create || f.slug != "todo" || f.name != "todo" || f.url != "https://airlock.example.com" || f.remote != "prod" || f.message != "Add reminders" || f.dir != "repo" {
		t.Fatalf("flags = %#v", f)
	}
	f, err = parseDeployFlags([]string{"--create", "--name", "Sales Deck", "repo"})
	if err != nil {
		t.Fatalf("parseDeployFlags with derived slug: %v", err)
	}
	if f.slug != "sales-deck" || f.name != "Sales Deck" {
		t.Fatalf("derived flags = %#v", f)
	}
	if _, err := parseDeployFlags([]string{"--create", "--slug", "todo", "--agent", "todo"}); err == nil {
		t.Fatal("--create with --agent returned nil error")
	}
	if _, err := parseDeployFlags([]string{"--remote", "bad remote"}); err == nil {
		t.Fatal("invalid --remote returned nil error")
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
			"go get github.com/airlockrun/agentsdk@v9.9.9",
			"go get -tool github.com/airlockrun/agentsdk/cmd/air@v9.9.9",
			"go mod tidy",
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

func TestDeviceLoginPendingState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	baseURL := "https://airlock.example.com/"
	pending := pendingDeviceLogin{
		DeviceCode:          "device-secret",
		UserCode:            "ABCD-EFGH",
		VerificationURL:     "https://airlock.example.com/device-login",
		ExpiresAt:           time.Now().Add(10 * time.Minute),
		PollIntervalSeconds: 3,
	}
	if err := savePendingDeviceLogin(baseURL, pending); err != nil {
		t.Fatalf("savePendingDeviceLogin: %v", err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if got := creds.PendingDeviceLogins["https://airlock.example.com"]; got.DeviceCode != pending.DeviceCode || got.UserCode != pending.UserCode {
		t.Fatalf("pending = %#v", got)
	}
	done, err := handleDeviceLoginPoll("https://airlock.example.com", &airlockv1.DeviceLoginPollResponse{
		Status:       "approved",
		AccessToken:  "access",
		RefreshToken: "refresh",
		User:         &airlockv1.User{Email: "dev@example.com"},
	})
	if err != nil || !done {
		t.Fatalf("handleDeviceLoginPoll done=%v err=%v", done, err)
	}
	creds, err = loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials after approve: %v", err)
	}
	if _, ok := creds.PendingDeviceLogins["https://airlock.example.com"]; ok {
		t.Fatalf("pending was not cleared: %#v", creds.PendingDeviceLogins)
	}
	if got := creds.Sessions["https://airlock.example.com"]; got.Email != "dev@example.com" || got.AccessToken != "access" || got.RefreshToken != "refresh" {
		t.Fatalf("session = %#v", got)
	}
}

func TestCmdLogoutRevokesAndClearsSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var sawLogout bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/logout" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		sawLogout = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", "refresh"); err != nil {
		t.Fatalf("saveLoginCredentials: %v", err)
	}
	if err := cmdLogout([]string{srv.URL}); err != nil {
		t.Fatalf("cmdLogout: %v", err)
	}
	if !sawLogout {
		t.Fatal("logout endpoint was not called")
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if _, ok := creds.Sessions[normalizeBaseURL(srv.URL)]; ok {
		t.Fatalf("session was not cleared: %#v", creds.Sessions)
	}
}

func TestAccessTokenClearsExpiredLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", "refresh"); err != nil {
		t.Fatalf("saveLoginCredentials: %v", err)
	}
	_, err := accessTokenForURL(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("accessTokenForURL returned nil error")
	}
	if want := "login expired for " + normalizeBaseURL(srv.URL) + "; run air login " + normalizeBaseURL(srv.URL); err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if _, ok := creds.Sessions[normalizeBaseURL(srv.URL)]; ok {
		t.Fatalf("expired session was not cleared: %#v", creds.Sessions)
	}
}

func TestEnsureToolchainProjectsCachedTools(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheDir, err := toolchainCacheDir()
	if err != nil {
		t.Fatalf("toolchainCacheDir: %v", err)
	}
	prefix := filepath.Join(t.TempDir(), ".airlock", "toolchain")
	mustWrite(t, tailwindCachePath(cacheDir), "tailwind")
	mustWrite(t, filepath.Join(daisyUICacheDir(cacheDir), "daisyui.mjs"), "daisyui")
	mustWrite(t, filepath.Join(daisyUICacheDir(cacheDir), "daisyui-theme.mjs"), "theme")

	if err := ensureToolchain(prefix); err != nil {
		t.Fatalf("ensureToolchain: %v", err)
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
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("projected skill file %s: %v", path, err)
		}
	}
}

func TestBuildStepsWritesBinaryOutsideRepository(t *testing.T) {
	output := filepath.Join(t.TempDir(), "agent")
	steps := buildSteps(".airlock/toolchain/bin/tailwindcss", output)
	if len(steps) != 4 {
		t.Fatalf("len(buildSteps) = %d, want 4", len(steps))
	}
	want := []string{"go", "build", "-buildvcs=false", "-o", output, "."}
	got := steps[len(steps)-1].cmd
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("build command = %q, want %q", got, want)
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

	_, err := resolveDeployTarget(context.Background(), srv.URL, "tok", "", agentRemoteBinding{AgentID: id, Slug: "stale-slug"})
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

	_, err := resolveDeployTarget(context.Background(), srv.URL, "tok", "todo", agentRemoteBinding{AgentID: boundID, Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), boundID) || !strings.Contains(err.Error(), realID) {
		t.Fatalf("resolveDeployTarget error = %v", err)
	}
}

func TestResolveDeployTargetRejectsSlugOnlyBinding(t *testing.T) {
	_, err := resolveDeployTarget(context.Background(), "https://airlock.example.com", "tok", "", agentRemoteBinding{Slug: "todo"})
	if err == nil || !strings.Contains(err.Error(), "no agent_id") || !strings.Contains(err.Error(), "--agent todo") {
		t.Fatalf("resolveDeployTarget error = %v", err)
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
