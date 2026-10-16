package scaffold

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

// TestScaffoldBuildsOffline verifies the generated unit suite, compilation and
// manifest without an app runtime, PostgreSQL, Docker or a provider account.
func TestScaffoldBuildsOffline(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sdk := filepath.Dir(wd)
	root := filepath.Dir(sdk)
	for _, path := range []string{sdk, filepath.Join(root, "goai"), filepath.Join(root, "sol")} {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
			t.Skipf("standalone checkout lacks sibling source %s", path)
		}
	}
	dir := t.TempDir()
	if err := Materialize(dir, ScaffoldData{AgentID: "offline-fixture", AgentSDKVersion: "v0.0.0", AgentBaseImage: "unused-offline"}); err != nil {
		t.Fatal(err)
	}
	module := fmt.Sprintf(`module agent

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
`, GoVersion, TemplVersion, sdk, filepath.Join(root, "goai"), filepath.Join(root, "sol"))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOWORK=off", "DOCKER_HOST=unix:///nonexistent", "AIRLOCK_API_URL=", "AIRLOCK_AGENT_ID=", "AIRLOCK_AGENT_TOKEN=", "AIRLOCK_DB_URL=", "TEST_DB_URL=")
	binary := filepath.Join(dir, "app")
	for _, args := range [][]string{
		{"mod", "tidy"}, {"tool", "templ", "generate"}, {"test", "-count=1", "./..."}, {"build", "-buildvcs=false", "-o", binary, "."},
	} {
		command := exec.Command("go", args...)
		command.Dir, command.Env = dir, env
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, output)
		}
	}
	command := exec.Command(binary)
	command.Env = append(env, "AIRLOCK_AGENT_MODE=manifest")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var manifest wire.AgentManifest
	if err := json.Unmarshal(output, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckAppRuntimeProtocol(manifest.RuntimeProtocol); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Routes) == 0 || len(manifest.StaticAssets) == 0 {
		t.Fatal("offline scaffold omitted app declarations")
	}
}
