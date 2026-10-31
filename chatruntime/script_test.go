package chatruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/tool"
)

type scriptBackend struct {
	backendFunc
	definitions []capability.Definition
}

func (b scriptBackend) Catalog(context.Context) ([]capability.Definition, error) {
	return b.definitions, nil
}

type cleanupFailureExecutor struct {
	executor
	closeErr error
}

func (e *cleanupFailureExecutor) Close() error { e.closes++; return e.closeErr }

func TestScriptCleanupPreservesExecutionClassification(t *testing.T) {
	cleanupErr := errors.New("private daemon diagnostic")
	executionErr := errors.New("execution interrupted; effects unknown")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "confirmed receipt"},
		{name: "unknown effects", err: executionErr},
		{name: "JavaScript failure", err: &jsexec.Exception{Name: "Error", Message: "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &cleanupFailureExecutor{closeErr: cleanupErr}
			e.execute = func(context.Context, string, jsexec.Invoker) (jsexec.Result, error) {
				return jsexec.Result{Output: json.RawMessage(`{"task":"developer:admitted"}`)}, tc.err
			}
			backend := scriptBackend{backendFunc: backendFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
				t.Fatal("script must not be replayed")
				return tool.Result{}, nil
			})}
			js := chatruntime.NewScriptTool(backend, func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }, "", true)
			result, err := js.Execute(t.Context(), json.RawMessage(`{"code":"return 42;","description":"test"}`), tool.CallOptions{})
			if e.calls != 1 || e.closes != 1 {
				t.Fatalf("executions=%d closes=%d", e.calls, e.closes)
			}
			if tc.err != nil {
				if !errors.Is(err, tc.err) || !errors.Is(err, cleanupErr) {
					t.Fatalf("lost execution/cleanup failure: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(result.Output, `"task":"developer:admitted"`) || !strings.Contains(result.Output, "do not replay") || result.Metadata["executor_cleanup_failed"] != true {
				t.Fatalf("confirmed receipt = %+v, %v", result, err)
			}
			if strings.Contains(result.Output, cleanupErr.Error()) {
				t.Fatal("private cleanup diagnostic leaked into result")
			}
		})
	}
}

func TestScriptAdmittedHandoffSurvivesCleanupFailure(t *testing.T) {
	path := capability.Local(capability.Air, "", "develop_app")
	admissions := 0
	backend := scriptBackend{
		definitions: []capability.Definition{{Path: path, Target: capability.Platform, InputSchema: json.RawMessage(`{"type":"object"}`)}},
		backendFunc: backendFunc(func(_ context.Context, in chatruntime.Invocation) (tool.Result, error) {
			admissions++
			if in.CapabilityID != path.ID() || in.ToolCallID != "handoff" {
				t.Fatalf("wrong handoff attribution: %+v", in)
			}
			return tool.Result{Output: `{"task":"developer:accepted-once"}`}, nil
		}),
	}
	e := &cleanupFailureExecutor{closeErr: context.DeadlineExceeded}
	e.execute = func(ctx context.Context, _ string, callback jsexec.Invoker) (jsexec.Result, error) {
		receipt, err := callback.Invoke(ctx, path.ID(), json.RawMessage(`[{"instruction":"change theme"}]`))
		return jsexec.Result{Output: receipt}, err
	}
	js := chatruntime.NewScriptTool(backend, func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }, "", true)
	result, err := js.Execute(t.Context(), json.RawMessage(`{"code":"return await platform.developApp({instruction:'change theme'});","description":"delegate"}`), tool.CallOptions{ToolCallID: "handoff"})
	if err != nil || admissions != 1 || !strings.Contains(result.Output, "developer:accepted-once") || result.Metadata["executor_cleanup_failed"] != true {
		t.Fatalf("admissions=%d result=%+v error=%v", admissions, result, err)
	}
}
