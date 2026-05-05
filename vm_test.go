package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/goai/tool"
)

type doubleIn struct {
	X int `json:"x"`
}

type doubleOut struct {
	Result int `json:"result"`
}

type runIDIn struct{}
type runIDOut struct {
	ID string `json:"id"`
}

func TestVM(t *testing.T) {
	t.Run("registered tool callable from JS", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(&Tool[doubleIn, doubleOut]{
			Name:        "double",
			Description: "Doubles a number.",
			Execute: func(ctx context.Context, in doubleIn) (doubleOut, error) {
				return doubleOut{Result: in.X * 2}, nil
			},
		})

		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), "double({x: 21}).result")
		if err != nil {
			t.Fatal(err)
		}
		if result != "42" {
			t.Fatalf("expected 42, got %s", result)
		}
	})

	t.Run("run context passed to Execute", func(t *testing.T) {
		a, _ := testAgent(t)
		var capturedRunID string
		a.RegisterTool(&Tool[runIDIn, runIDOut]{
			Name:        "get_run_id",
			Description: "Returns run ID.",
			Execute: func(ctx context.Context, in runIDIn) (runIDOut, error) {
				r := runFromContext(ctx)
				if r != nil {
					capturedRunID = r.id
					return runIDOut{ID: r.id}, nil
				}
				return runIDOut{}, nil
			},
		})

		run := newRun(a, "run-42", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), "get_run_id().id")
		if err != nil {
			t.Fatal(err)
		}
		if result != "run-42" {
			t.Fatalf("expected run-42, got %s", result)
		}
		if capturedRunID != "run-42" {
			t.Fatalf("run was not passed correctly")
		}
	})

	// Regression: user-registered tools dispatched from the VM must
	// receive a ctx with the caller attached, so their own
	// CheckFileAccess calls see the run's resolved access level instead
	// of the AccessPublic zero-value (which would deny every write).
	t.Run("user tool gets caller in ctx", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterDirectory("downloads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		var checkErr error
		a.RegisterTool(&Tool[runIDIn, runIDOut]{
			Name:        "probe_write",
			Description: "Probes write access on downloads/.",
			Execute: func(ctx context.Context, in runIDIn) (runIDOut, error) {
				checkErr = a.CheckFileAccess(ctx, "downloads/x.bin", OpWrite)
				return runIDOut{}, nil
			},
		})

		run := newRun(a, "run-1", "", "", context.Background())
		run.callerAccess = AccessUser
		if _, err := executeJS(run.vmRuntime(), "probe_write()"); err != nil {
			t.Fatal(err)
		}
		if checkErr != nil {
			t.Fatalf("CheckFileAccess from user tool: %v", checkErr)
		}
	})

	t.Run("log binding", func(t *testing.T) {
		a, _ := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `log("hello from JS")`)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.logs) != 1 || run.logs[0].Message != "hello from JS" || run.logs[0].Level != LogLevelInfo {
			t.Fatalf("expected info log entry, got %v", run.logs)
		}
	})

	t.Run("deleteFile calls backend", func(t *testing.T) {
		a, mock := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `deleteFile("uploads/a.txt")`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/storage/uploads/a.txt")
		if len(reqs) != 1 {
			t.Fatalf("expected 1 delete request, got %d", len(reqs))
		}
		if reqs[0].Method != "DELETE" {
			t.Fatalf("expected DELETE, got %s", reqs[0].Method)
		}
	})

	t.Run("listDir returns array", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `JSON.stringify(listDir("uploads/"))`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "[]" {
			t.Fatalf("expected empty array, got %s", result)
		}
	})

	t.Run("writeFile calls backend with absolute path", func(t *testing.T) {
		a, mock := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `writeFile("uploads/test.txt", "hello", "text/plain")`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/storage/uploads/test.txt")
		if len(reqs) != 1 || reqs[0].Method != "PUT" {
			t.Fatalf("expected PUT to storage at uploads/test.txt, got %v", reqs)
		}
	})

	t.Run("readFile calls backend with absolute path", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `readFile("uploads/test.txt")`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "mock-file-content" {
			t.Fatalf("expected mock-file-content, got %s", result)
		}
	})

	t.Run("readBytes returns a usable Uint8Array", func(t *testing.T) {
		// Regression: readBytes used to return a raw ArrayBuffer, which
		// isn't iterable and has no .length, so `Array.from(b.slice(...))`
		// silently produced []. The fix wraps in Uint8Array via the
		// global TypedArray constructor — invoke as a constructor (not
		// a plain function), or you get
		// "TypeError: Constructor TypedArray requires 'new'".
		a, _ := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		// Mock backend returns the literal string "mock-file-content"
		// (17 bytes). Verify .length is correct, indexed access works,
		// .slice returns a typed array with the right bytes, and
		// Array.from materializes a real number array.
		result, err := executeJS(run.vmRuntime(), `
			var b = readBytes("uploads/test.txt");
			JSON.stringify({
				ctor: b.constructor.name,
				length: b.length,
				first: b[0],
				slice: Array.from(b.slice(0, 4)),
				full: Array.from(b),
			});
		`)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Ctor   string `json:"ctor"`
			Length int    `json:"length"`
			First  int    `json:"first"`
			Slice  []int  `json:"slice"`
			Full   []int  `json:"full"`
		}
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Fatalf("decode: %v\nraw: %s", err, result)
		}
		if parsed.Ctor != "Uint8Array" {
			t.Errorf("constructor.name = %q, want Uint8Array", parsed.Ctor)
		}
		const expected = "mock-file-content"
		if parsed.Length != len(expected) {
			t.Errorf("length = %d, want %d", parsed.Length, len(expected))
		}
		if parsed.First != int(expected[0]) {
			t.Errorf("first byte = %d, want %d (%q)", parsed.First, expected[0], expected[0])
		}
		if len(parsed.Slice) != 4 {
			t.Errorf("slice length = %d, want 4", len(parsed.Slice))
		}
		if len(parsed.Full) != len(expected) {
			t.Errorf("Array.from(bytes) length = %d, want %d", len(parsed.Full), len(expected))
		}
		for i, b := range parsed.Full {
			if b != int(expected[i]) {
				t.Errorf("byte[%d] = %d, want %d", i, b, expected[i])
				break
			}
		}
	})

	t.Run("printToUser calls backend", func(t *testing.T) {
		a, mock := testAgent(t)
		run := newRun(a, "run-1", "", "conv-1", context.Background())
		_, err := executeJS(run.vmRuntime(), `printToUser({type: "text", text: "hello"})`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/print")
		if len(reqs) != 1 {
			t.Fatalf("expected 1 print request, got %d", len(reqs))
		}
	})

	t.Run("printToUser accepts array", func(t *testing.T) {
		a, mock := testAgent(t)
		run := newRun(a, "run-1", "", "conv-1", context.Background())
		_, err := executeJS(run.vmRuntime(), `printToUser([{type: "text", text: "hi"}, {type: "image", source: "img.png"}])`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/print")
		if len(reqs) != 1 {
			t.Fatalf("expected 1 print request, got %d", len(reqs))
		}
	})

	t.Run("statFile returns FileInfo with absolute path", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `var fi = statFile("uploads/test.txt"); fi.path + ":" + fi.size`)
		if err != nil {
			t.Fatal(err)
		}
		// Mock backend returns "tmp/test.txt" — path comes from the
		// server response, the test only verifies fields are surfaced.
		if result != "tmp/test.txt:42" {
			t.Fatalf("expected mock path:size, got %s", result)
		}
	})
}

// TestRunJSInterruptOnCtxCancel verifies that cancelling the run's ctx
// aborts a runaway JS loop via goja.Runtime.Interrupt — without this, an
// infinite while(true) in LLM-generated code spins at 100% CPU forever.
func TestRunJSInterruptOnCtxCancel(t *testing.T) {
	a, _ := testAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := newRun(a, "run-1", "", "", ctx)

	// Cancel after a short delay so the JS is mid-loop when interrupted.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	runJS := buildRunJSTool(a, run)
	input, _ := json.Marshal(runJSInput{Code: "while(true){}"})

	type out struct {
		res tool.Result
		err error
	}
	resCh := make(chan out, 1)
	go func() {
		r, err := runJS.Execute(ctx, input, tool.CallOptions{ToolCallID: "tc-1"})
		resCh <- out{r, err}
	}()

	select {
	case r := <-resCh:
		// run_js swallows the executeJS error into the Output string
		// ("Error: ..."). Either an error returned OR an error-prefixed
		// output is acceptable — both prove the loop was interrupted.
		if r.err == nil && !strings.Contains(r.res.Output, "Error:") {
			t.Fatalf("expected interruption error, got Output=%q err=%v", r.res.Output, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run_js did not return within 2s after ctx cancel — interrupt did not fire")
	}
}
