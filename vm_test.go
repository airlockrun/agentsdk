package agentsdk

import (
	"context"
	"testing"
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

	t.Run("log binding", func(t *testing.T) {
		a, _ := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `log("hello from JS")`)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.logs) != 1 || run.logs[0] != "hello from JS" {
			t.Fatalf("expected log entry, got %v", run.logs)
		}
	})

	t.Run("copyFile calls backend", func(t *testing.T) {
		a, mock := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `copyFile("a.txt", "b.txt")`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/storage/copy")
		if len(reqs) != 1 {
			t.Fatalf("expected 1 copy request, got %d", len(reqs))
		}
		if reqs[0].Method != "POST" {
			t.Fatalf("expected POST, got %s", reqs[0].Method)
		}
	})

	t.Run("removeFile calls backend", func(t *testing.T) {
		a, mock := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `removeFile("a.txt")`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/storage/a.txt")
		if len(reqs) != 1 {
			t.Fatalf("expected 1 delete request, got %d", len(reqs))
		}
		if reqs[0].Method != "DELETE" {
			t.Fatalf("expected DELETE, got %s", reqs[0].Method)
		}
	})

	t.Run("listFiles returns array", func(t *testing.T) {
		a, _ := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `JSON.stringify(listFiles())`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "[]" {
			t.Fatalf("expected empty array, got %s", result)
		}
	})

	t.Run("writeFile calls backend", func(t *testing.T) {
		a, mock := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		_, err := executeJS(run.vmRuntime(), `writeFile("test.txt", "hello", "text/plain")`)
		if err != nil {
			t.Fatal(err)
		}
		reqs := mock.RequestsByPath("/api/agent/storage/test.txt")
		if len(reqs) != 1 || reqs[0].Method != "PUT" {
			t.Fatalf("expected PUT to storage, got %v", reqs)
		}
	})

	t.Run("readFile calls backend", func(t *testing.T) {
		a, _ := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `readFile("test.txt")`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "mock-file-content" {
			t.Fatalf("expected mock-file-content, got %s", result)
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

	t.Run("fileInfo returns metadata", func(t *testing.T) {
		a, _ := testAgent(t)
		run := newRun(a, "run-1", "", "", context.Background())
		result, err := executeJS(run.vmRuntime(), `var fi = fileInfo("test.txt"); fi.key + ":" + fi.size`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "test.txt:42" {
			t.Fatalf("expected test.txt:42, got %s", result)
		}
	})
}
