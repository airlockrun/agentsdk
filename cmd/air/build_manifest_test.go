package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckAppManifest(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	// Compile a real SDK app once. Registration is conditional and delegated,
	// so the check must inspect the artifact rather than count source calls.
	if err := os.WriteFile(source, []byte(`package main
import (
 "context"
 "fmt"
 "os"
 "github.com/airlockrun/agentsdk"
 "github.com/airlockrun/goai/tool"
)
type In struct { Query string `+"`json:\"query\"`"+` }
type Out struct { Result string `+"`json:\"result\"`"+` }
func register(a *agentsdk.Agent) {
 a.RegisterTool(tool.Typed[In, Out]("search").Description("Search items.").Execute(
  func(ctx context.Context, in In) (Out, error) { return Out{Result: in.Query}, nil },
 ).Build(), agentsdk.AccessUser)
}
func main() {
 if os.Getenv("AIRLOCK_AGENT_MODE") != "manifest" { panic("runtime must not start") }
 mode := os.Getenv("AIR_TEST_MANIFEST_CASE")
 switch mode {
 case "failed": fmt.Fprintln(os.Stderr, "definition failed"); os.Exit(2)
 case "invalid": fmt.Print("not JSON"); return
 case "empty": return
 case "incomplete": fmt.Print("{}"); return
 case "trailing": fmt.Print("{} {}"); return
 }
 a := agentsdk.New(agentsdk.Config{Description: "Manifest test", Icon: "🔎"})
 if mode == "tools" { register(a) }
 a.Serve()
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "app")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.Command("go", "build", "-buildvcs=false", "-o", binary, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build manifest fixture: %v\n%s", err, output)
	}
	for _, name := range []string{"AIRLOCK_API_URL", "AIRLOCK_AGENT_ID", "AIRLOCK_AGENT_TOKEN", "AIRLOCK_DB_URL"} {
		t.Setenv(name, "")
	}
	for _, tt := range []struct {
		name    string
		warning bool
		wantErr string
	}{
		{name: "no-tools", warning: true},
		{name: "tools"},
		{name: "failed", wantErr: "definition failed"},
		{name: "invalid", wantErr: "decode app manifest"},
		{name: "empty", wantErr: "stdout is empty"},
		{name: "incomplete", wantErr: "app manifest:"},
		{name: "trailing", wantErr: "decode app manifest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AIR_TEST_MANIFEST_CASE", tt.name)
			var warnings bytes.Buffer
			err := checkAppManifest(dir, binary, &warnings)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if tt.warning {
				for _, want := range []string{"WARNING: No tools registered.", "Chat cannot invoke", "RegisterTool", "UI-only"} {
					if !strings.Contains(warnings.String(), want) {
						t.Errorf("warning = %q, missing %q", warnings.String(), want)
					}
				}
			} else if warnings.Len() != 0 {
				t.Fatalf("unexpected warning: %s", &warnings)
			}
		})
	}
}
