package chatruntime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/tool"
)

type argsExecutor struct {
	code   string
	args   json.RawMessage
	called int
}

func TestSavedHomeScriptUsesAuthorizedCatalogRead(t *testing.T) {
	executor := &argsExecutor{}
	files := capability.Definition{Path: capability.Qualified(capability.Host, "", "", "files"), Target: capability.Platform, InputSchema: json.RawMessage(`{}`)}
	r := &runtime{ctx: t.Context(), input: Input{AutoConfirm: true, Capabilities: []capability.Definition{files}, Backend: testBackend(func(_ context.Context, in Invocation) (tool.Result, error) {
		var params map[string]string
		if json.Unmarshal(in.Input, &params) != nil || params["operation"] != "read" || params["path"] != "/home/Scripts/a.js" {
			t.Fatal("home script bypassed catalog resolver")
		}
		return tool.Result{Output: `{"text":"return args.value;","file":{"path":"/home/Scripts/a.js","contentId":"content-version"}}`}, nil
	}), ExecutorFactory: func(context.Context, []capability.Definition) (jsexec.Session, error) { return executor, nil }}}
	out, err := r.scriptTool().Execute(t.Context(), json.RawMessage(`{"path":"/home/Scripts/a.js","args":{"value":7},"description":"Read home script"}`), tool.CallOptions{ToolCallID: "home"})
	if err != nil || out.Metadata["source_content_id"] != "content-version" || executor.called != 1 {
		t.Fatalf("home script=%+v error=%v", out, err)
	}
}

func (e *argsExecutor) Execute(context.Context, string, jsexec.Invoker) (jsexec.Result, error) {
	return jsexec.Result{}, errors.New("structured args required")
}
func (e *argsExecutor) Close() error { return nil }
func (e *argsExecutor) ExecuteWithArgs(_ context.Context, code string, args json.RawMessage, _ jsexec.Invoker) (jsexec.Result, error) {
	e.code = code
	e.args = append(json.RawMessage(nil), args...)
	e.called++
	return jsexec.Result{Output: json.RawMessage(`{"ok":true}`)}, nil
}

func TestSavedScriptPolicyAndHash(t *testing.T) {
	var read capability.Definition
	for _, d := range capability.Fixed() {
		if d.Path.ID() == "air//file_read" {
			read = d
		}
	}
	for _, tc := range []struct {
		name, input string
		allow       bool
		loadErr     bool
		valid       bool
	}{
		{"allowed", `{"path":"scripts/a.js","args":{"value":"\";throw Error('injected');//"},"description":"Execute saved"}`, true, false, true},
		{"hash mismatch", `{"path":"scripts/a.js","args":{},"description":"Execute saved","expected_sha256":"wrong"}`, true, false, false},
		{"no read grant", `{"path":"scripts/a.js","args":{},"description":"Execute saved"}`, false, false, false},
		{"traversal", `{"path":"scripts/../secret.js","args":{},"description":"Execute saved"}`, true, false, false},
		{"not TS", `{"path":"scripts/a.ts","args":{},"description":"Execute saved"}`, true, false, false},
		{"args array", `{"path":"scripts/a.js","args":[],"description":"Execute saved"}`, true, false, false},
		{"ACL denied", `{"path":"scripts/a.js","args":{},"description":"Execute saved"}`, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &argsExecutor{}
			r := &runtime{ctx: t.Context(), input: Input{AutoConfirm: true, Backend: testBackend(func(_ context.Context, in Invocation) (tool.Result, error) {
				if in.CapabilityID != read.Path.ID() {
					t.Fatal("non-allowlisted file loader")
				}
				if tc.loadErr {
					return tool.Result{}, errors.New("denied")
				}
				return tool.Result{Output: "return args.value;"}, nil
			}), ExecutorFactory: func(context.Context, []capability.Definition) (jsexec.Session, error) { return executor, nil }}}
			if tc.allow {
				r.input.Capabilities = []capability.Definition{read}
			}
			if !tc.allow {
				if _, ok := r.tools()["run_js_file"]; ok {
					t.Fatal("script tool advertised without read")
				}
			}
			result, err := r.scriptTool().Execute(t.Context(), json.RawMessage(tc.input), tool.CallOptions{ToolCallID: "saved"})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
			if !tc.valid && executor.called != 0 {
				t.Fatal("rejected script executed")
			}
			if tc.valid && (executor.code != "return args.value;" || len(executor.args) == 0 || result.Metadata["source_path"] != "scripts/a.js" || result.Metadata["source_sha256"] == nil) {
				t.Fatal("structured args/source metadata lost")
			}
		})
	}
}
