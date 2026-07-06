package scaffold

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestScaffoldBuildsAndStarts verifies that the scaffold output compiles
// and that the resulting binary starts and serves /health.
func TestScaffoldBuildsAndStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Resolve the sibling lib source trees so the scaffolded agent builds
	// against the agentsdk/goai/sol we're editing (via replace directives),
	// not a published proxy version — this test validates the scaffold against
	// the SDK source in this checkout.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	agentsdkPath := filepath.Dir(wd)     // .../agentsdk (scaffold's parent)
	hqRoot := filepath.Dir(agentsdkPath) // monorepo root holding sibling libs
	goaiPath := filepath.Join(hqRoot, "goai")
	solPath := filepath.Join(hqRoot, "sol")
	for _, p := range []string{agentsdkPath, goaiPath, solPath} {
		if _, err := os.Stat(filepath.Join(p, "go.mod")); err != nil {
			t.Skipf("sibling lib not found at %s; skipping (needs the monorepo layout)", p)
		}
	}

	// Materialize scaffold.
	dir := t.TempDir()
	data := ScaffoldData{
		AgentID:         "test-agent-build",
		Module:          "agent",
		GoVersion:       "1.26",
		AgentSDKVersion: "v1.0.0",
		AgentBaseImage:  "airlock-agent-base",
	}
	if err := Materialize(dir, data); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Overwrite go.mod: require the libs + templ, expose templ as a tool,
	// and replace the owned libs
	// with local source so their resolution needs no network or published
	// version. templ and the transitive external deps still resolve via the
	// module cache / proxy. GoVersion + TemplVersion come from the scaffold's
	// own consts, so this fixture can't drift from what it ships.
	goMod := fmt.Sprintf(`module agent

go %s

require (
	github.com/a-h/templ %s
	github.com/airlockrun/agentsdk v0.0.0
	github.com/airlockrun/goai v0.0.0
	github.com/airlockrun/sol v0.0.0
)

replace github.com/airlockrun/agentsdk => %s
replace github.com/airlockrun/goai => %s
replace github.com/airlockrun/sol => %s

tool github.com/a-h/templ/cmd/templ
`, GoVersion, TemplVersion, agentsdkPath, goaiPath, solPath)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Disable any ambient go.work (the hq monorepo has one) so resolution
	// goes purely through go.mod + the proxy. CI doesn't have a workspace
	// either, this keeps the two paths identical.
	env := append(os.Environ(), "GOWORK=off")

	// `go build` won't auto-populate go.sum; tidy first.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = env
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy failed:\n%s", out)
	}

	// --- Step 1: Generate templ + Tailwind + Build ---
	templGen := exec.Command("go", "tool", "templ", "generate")
	templGen.Dir = dir
	templGen.Env = env
	if out, err := templGen.CombinedOutput(); err != nil {
		t.Fatalf("templ generate failed:\n%s", out)
	}

	// tailwindcss is optional in CI / dev — the scaffold ships a
	// placeholder views/static/app.css the //go:embed reads, so the
	// agent compiles without it. When the repo-local binary exists, we run
	// it so the test exercises the same chain as the prod Docker build.
	tailwindPath := filepath.Join(dir, ".airlock", "toolchain", "bin", "tailwindcss")
	if _, err := os.Stat(tailwindPath); err == nil {
		tw := exec.Command(tailwindPath,
			"-i", "styles/app.css",
			"-o", "views/static/app.css",
			"--minify")
		tw.Dir = dir
		tw.Env = env
		if out, err := tw.CombinedOutput(); err != nil {
			t.Fatalf("tailwindcss compile failed:\n%s", out)
		}
	} else {
		t.Log("repo-local tailwindcss not installed; using scaffold placeholder views/static/app.css")
	}

	binPath := filepath.Join(dir, "agent")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = dir
	build.Env = env
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed:\n%s", out)
	}

	// --- Step 2: Start with mock Airlock ---
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Sync endpoint must return a valid SyncResponse: agentsdk's
		// applySyncResponse panics at boot on an empty
		// PromptData.AgentRouteURL (the "airlock newer than agentsdk"
		// guard), which would crash the agent before it binds /health.
		// Other endpoints just need a 200 with parseable JSON.
		if r.URL.Path == "/api/agent/sync" {
			w.Write([]byte(`{"promptData":{"agentDashboardUrl":"http://airlock.test/agents/test-agent","agentRouteUrl":"http://agent.test"}}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer mock.Close()

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(binPath)
	cmd.Env = []string{
		"AIRLOCK_AGENT_ID=test-agent",
		"AIRLOCK_API_URL=" + mock.URL,
		"AIRLOCK_AGENT_TOKEN=test-token",
		"AIRLOCK_ADDR=" + addr,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	defer cmd.Process.Kill()

	// Poll /health until it responds or times out.
	healthURL := fmt.Sprintf("http://%s/health", addr)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthURL)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.Status != "ok" {
			t.Fatalf("expected status ok, got %q", body.Status)
		}
		return // success
	}
	t.Fatal("agent did not start within 5 seconds")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
