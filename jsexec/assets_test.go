package jsexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDenoSecurityGate verifies permission-based isolation. Module loading is
// permitted; privileged operations must still be denied.
func TestDenoSecurityGate(t *testing.T) {
	if os.Getenv("JSEXEC_DENO_PROBE") != "1" {
		t.Skip("set JSEXEC_DENO_PROBE=1 to run the stock-Deno feasibility gate")
	}
	source, err := ProbeAssets.ReadFile("probe/feasibility.mjs")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := ProbeAssets.ReadFile("probe/local.mjs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local.mjs")
	if err := os.WriteFile(path, fixture, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"--network", "none", "--memory", "256m", "--memory-swap", "256m",
		"--cpus", "1", "--pids-limit", "64", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--read-only", "--user", "65532:65532",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--env", "DENO_DIR=/tmp/deno",
		"--mount", "type=bind,src="+path+",dst=/probe/local.mjs,readonly",
		"--entrypoint", "/usr/bin/deno", DenoImage,
		"run", "--no-config", "--no-lock", "--no-remote", "--no-npm", "--no-code-cache", "--no-prompt",
		"--deny-read", "--deny-write", "--deny-net", "--deny-env", "--deny-sys", "--deny-run", "--deny-ffi", "--deny-import", "-")
	cmd.Stdin = bytes.NewReader(source)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Deno probe: %v\n%s\n%s", err, out, stderr.String())
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	count := 0
	for scanner.Scan() {
		var observation struct {
			Name    string          `json:"name"`
			Allowed bool            `json:"allowed"`
			Value   json.RawMessage `json:"value"`
			Error   string          `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &observation); err != nil {
			t.Fatal(err)
		}
		count++
		t.Run(observation.Name, func(t *testing.T) {
			t.Logf("allowed=%v value=%s error=%s", observation.Allowed, observation.Value, observation.Error)
			switch observation.Name {
			case "async_retained_state":
				if !observation.Allowed || string(observation.Value) != "42" {
					t.Fatal("async retained-state proof failed")
				}
			case "bootstrap_global_tamper":
				if !observation.Allowed || string(observation.Value) != `"tampered"` {
					t.Fatal("same-realm mutability probe changed; review bootstrap assumptions")
				}
			case "data_import", "computed_data_import", "function_import", "eval_import", "worker_data_import":
				if !observation.Allowed || string(observation.Value) != "42" {
					t.Fatal("permitted module/worker probe failed")
				}
			case "node_import":
				if !observation.Allowed || string(observation.Value) != `"function"` {
					t.Fatal("permitted node import failed")
				}
			default:
				if observation.Allowed {
					t.Fatal("SECURITY GATE: forbidden operation succeeded under the full permission deny set")
				}
				if observation.Error == "" || strings.Contains(observation.Error, "timeout") {
					t.Fatal("probe did not establish a permission or loader rejection")
				}
				if !strings.Contains(observation.Error, "Requires ") && !strings.Contains(observation.Error, "--no-npm") {
					t.Fatal("failure is not a verified permission or loader-policy rejection")
				}
			}
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 19 {
		t.Fatalf("got %d observations, want 19", count)
	}
}
