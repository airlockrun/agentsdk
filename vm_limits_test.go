package agentsdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/goai/tool"
	"github.com/dop251/goja"
)

// capJSBytes is the single chokepoint for L1 (per-crossing byte cap). Test
// it directly: at/under the cap it is a no-op; over the cap it aborts the
// JS call by panicking a goja *GoError with an actionable message.
func TestCapJSBytes(t *testing.T) {
	vm := goja.New()

	t.Run("under cap is a no-op", func(t *testing.T) {
		capJSBytes(vm, "fileRead", maxJSValueBytes-1)
		capJSBytes(vm, "fileRead", maxJSValueBytes) // exactly at cap is allowed
	})

	t.Run("over cap aborts with actionable error", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected capJSBytes to panic over the cap")
			}
			v, ok := r.(goja.Value)
			if !ok {
				t.Fatalf("panic value is %T, want goja.Value", r)
			}
			msg := v.ToString().String()
			for _, want := range []string{"fileRead", "too large for run_js", "storage path"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q missing %q", msg, want)
				}
			}
		}()
		capJSBytes(vm, "fileRead", maxJSValueBytes+1)
	})
}

type bigIn struct {
	N int `json:"n"`
}
type bigOut struct {
	Blob string `json:"blob"`
}

// End-to-end: a tool whose return exceeds the cap fails the run_js call
// with the size error instead of materializing the bytes in the goja heap.
func TestVM_ToolReturnSizeCap(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(tool.Typed[bigIn, bigOut]("make_blob").
		Description("Returns a blob of n bytes.").
		Execute(func(ctx context.Context, in bigIn) (bigOut, error) {
			return bigOut{Blob: strings.Repeat("a", in.N)}, nil
		}).Build(), AccessUser)
	run := newRun(a, "run-1", "", "", context.Background())

	t.Run("under cap succeeds", func(t *testing.T) {
		out, err := executeJS(run.vmRuntime(), `make_blob({n: 1024}).blob.length`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "1024" {
			t.Fatalf("got %s, want 1024", out)
		}
	})

	t.Run("over cap fails fast with size error", func(t *testing.T) {
		_, err := executeJS(run.vmRuntime(),
			`make_blob({n: `+itoa(maxJSValueBytes+1)+`}).blob.length`)
		if err == nil {
			t.Fatal("expected a size-cap error, got nil")
		}
		if !strings.Contains(err.Error(), "too large for run_js") {
			t.Fatalf("error %q does not mention the size cap", err)
		}
	})
}

// L4 — amplifier-builtin guards. Each "blows up" case must return a clean
// catchable error (RangeError), NOT crash the process; each "normal" case
// must keep working.
func TestVM_AmplifierGuards(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "run-1", "", "", context.Background())
	vm := run.vmRuntime()

	blocked := []struct{ name, code string }{
		{"repeat", `"x".repeat(1e10)`},
		{"padStart", `"x".padStart(1e10)`},
		{"padEnd", `"x".padEnd(1e10)`},
		{"ArrayBuffer", `new ArrayBuffer(1e10)`},
		{"Uint8Array", `new Uint8Array(1e10)`},
		{"Float64Array", `new Float64Array(1e10)`},
		{"Array.fill", `new Array(1e9).fill(0)`},
	}
	for _, tc := range blocked {
		t.Run("blocked/"+tc.name, func(t *testing.T) {
			_, err := executeJS(vm, tc.code)
			if err == nil {
				t.Fatalf("%s: expected a RangeError, got nil (guard missing)", tc.code)
			}
			if !strings.Contains(err.Error(), "too large for run_js") {
				t.Fatalf("%s: error %q does not mention the cap", tc.code, err)
			}
		})
	}

	normal := []struct{ name, code, want string }{
		{"repeat", `"ab".repeat(3)`, "ababab"},
		{"padStart", `"5".padStart(3, "0")`, "005"},
		{"Uint8Array len", `new Uint8Array(16).length + ""`, "16"},
		{"Uint8Array from array", `new Uint8Array([1,2,3]).length + ""`, "3"},
		{"ArrayBuffer ok", `new ArrayBuffer(64).byteLength + ""`, "64"},
		{"fill ok", `[1,2,3].fill(9).join(",")`, "9,9,9"},
		{"instanceof preserved", `(new Uint8Array(4)) instanceof Uint8Array ? "y" : "n"`, "y"},
	}
	for _, tc := range normal {
		t.Run("normal/"+tc.name, func(t *testing.T) {
			got, err := executeJS(vm, tc.code)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.code, err)
			}
			if got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// withTinyGuardLimits shrinks the L3b tunables for fast, deterministic
// tests and restores them. Tests using it must not run in parallel.
func withTinyGuardLimits(t *testing.T, cpu, interval time.Duration) {
	t.Helper()
	oc, oi := jsCPULimit, jsMonitorInterval
	jsCPULimit, jsMonitorInterval = cpu, interval
	t.Cleanup(func() {
		jsCPULimit, jsMonitorInterval = oc, oi
	})
}

func TestStartJSGuard_CPUSpinTrips(t *testing.T) {
	withTinyGuardLimits(t, 100*time.Millisecond, 5*time.Millisecond)
	vm := goja.New()
	vm.SetMaxCallStackSize(maxJSCallStackSize)
	gw := &goWall{}

	stop := startJSGuard(context.Background(), vm, gw)
	defer stop()

	done := make(chan error, 1)
	go func() { _, err := vm.RunString(`while(true){}`); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "CPU time limit") {
			t.Fatalf("expected CPU-limit interrupt, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CPU spin was not interrupted — guard not firing")
	}
}

// A long blocking Go call (a download) must NOT be charged as JS CPU: the
// guard subtracts go-call time, so a 250 ms call under an 80 ms CPU budget
// still completes cleanly.
func TestStartJSGuard_LongGoCallNotChargedAsCPU(t *testing.T) {
	withTinyGuardLimits(t, 80*time.Millisecond, 5*time.Millisecond)
	vm := goja.New()
	vm.SetMaxCallStackSize(maxJSCallStackSize)
	gw := &goWall{}
	vm.Set("slowDownload", func(goja.FunctionCall) goja.Value {
		gw.enter()
		time.Sleep(250 * time.Millisecond) // legit blocking I/O
		gw.exit()
		return vm.ToValue("payload")
	})

	stop := startJSGuard(context.Background(), vm, gw)
	defer stop()

	done := make(chan struct {
		v   goja.Value
		err error
	}, 1)
	go func() {
		v, err := vm.RunString(`slowDownload()`)
		done <- struct {
			v   goja.Value
			err error
		}{v, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("long Go call was wrongly interrupted: %v", r.err)
		}
		if r.v == nil || r.v.String() != "payload" {
			t.Fatalf("got %v, want payload", r.v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call never returned")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
