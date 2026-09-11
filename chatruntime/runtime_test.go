package chatruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/session"
)

type backendFunc func(context.Context, chatruntime.Invocation) (tool.Result, error)

func (f backendFunc) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	return f(ctx, in)
}

type executor struct {
	calls, closes int
	execute       func(context.Context, string, jsexec.Invoker) (jsexec.Result, error)
}

func (e *executor) Execute(ctx context.Context, code string, i jsexec.Invoker) (jsexec.Result, error) {
	e.calls++
	if e.execute != nil {
		return e.execute(ctx, code, i)
	}
	return jsexec.Result{Output: json.RawMessage(`42`)}, nil
}
func (e *executor) Close() error { e.closes++; return nil }

func input(model stream.Model) chatruntime.Input {
	return chatruntime.Input{
		Message: "help", Model: model, ModelLimits: session.ModelLimits{Input: 80000, Output: 4096},
		MaxSteps: 10, SessionStore: &agenttest.MemoryStore{}, Sink: &agenttest.Events{},
		Backend: backendFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
			return tool.Result{}, errors.New("unexpected backend invocation")
		}),
		ExecutorFactory: func(context.Context, []capability.Definition) (jsexec.Session, error) {
			return nil, errors.New("unexpected allocation")
		},
	}
}

func jsCall(id string, confirm bool) []stream.Event {
	return testutil.MockToolCallResponse(id, "run_js", map[string]any{"code": "return 42;", "description": "Calculate the answer", "request_confirmation": confirm}, testutil.MockUsage(10, 10))
}
func reply() []stream.Event { return testutil.MockTextResponse("answer", testutil.MockUsage(10, 10)) }

func TestRunLazyLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		responses            [][]stream.Event
		wantCalls, wantAlloc int
		status               sol.RunStatus
	}{
		{"text", [][]stream.Event{reply()}, 0, 0, sol.RunCompleted},
		{"two serial scripts", [][]stream.Event{jsCall("first", false), jsCall("second", false), reply()}, 2, 1, sol.RunCompleted},
		{"approval before allocation", [][]stream.Event{jsCall("gate", true)}, 0, 0, sol.RunSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: tc.responses})
			in := input(model)
			e, allocations := &executor{}, 0
			in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { allocations++; return e, nil }
			result, err := chatruntime.Run(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.status || e.calls != tc.wantCalls || allocations != tc.wantAlloc || e.closes != tc.wantAlloc {
				t.Fatalf("status=%s execute=%d allocate=%d close=%d", result.Status, e.calls, allocations, e.closes)
			}
			messages, err := in.SessionStore.Load(t.Context())
			if err != nil || len(messages) < 2 {
				t.Fatalf("persisted messages: %v %v", messages, err)
			}
			if len(in.Sink.(*agenttest.Events).Snapshot()) == 0 {
				t.Fatal("no forwarded events")
			}
		})
	}
}

func TestRunRejectsMalformedMCPSchemaBeforeModel(t *testing.T) {
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: reply()})
	in := input(model)
	in.Capabilities = []capability.Definition{{
		Path: capability.Local(capability.MCP, "external", "search"), Target: capability.Platform,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":42}`),
	}}
	result, err := chatruntime.Run(t.Context(), in)
	if result != nil || err == nil || !strings.Contains(err.Error(), "additionalProperties") || len(model.DoStreamCalls) != 0 {
		t.Fatalf("result=%+v err=%v model calls=%d", result, err, len(model.DoStreamCalls))
	}
	messages, err := in.SessionStore.Load(t.Context())
	if err != nil || len(messages) != 0 {
		t.Fatalf("invalid schema wrote history: %v, %v", messages, err)
	}
}

func TestRunPermissionResume(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "deny"}[approved], func(t *testing.T) {
			in := input(testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{jsCall("gate", true)}}))
			first, err := chatruntime.Run(t.Context(), in)
			if err != nil || first.Status != sol.RunSuspended {
				t.Fatalf("suspension: %+v %v", first, err)
			}
			in.Resume, in.Approved = first.SuspensionContext, &approved
			in.Model = testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: reply()})
			e := &executor{}
			in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
			result, err := chatruntime.Run(t.Context(), in)
			if err != nil || result.Status != sol.RunCompleted {
				t.Fatalf("resume: %+v %v", result, err)
			}
			want := 0
			if approved {
				want = 1
			}
			if e.calls != want || e.closes != want {
				t.Fatalf("execute=%d close=%d want=%d", e.calls, e.closes, want)
			}
		})
	}
}

func TestRunOrderedBatchResuspendsBeforeNextScript(t *testing.T) {
	batch := append(jsCall("first", true)[:1], jsCall("second", true)...)
	in := input(testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: batch}))
	first, err := chatruntime.Run(t.Context(), in)
	if err != nil || first.Status != sol.RunSuspended {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	approved := true
	in.Resume, in.Approved = first.SuspensionContext, &approved
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: reply()})
	in.Model = model
	e := &executor{}
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
	second, err := chatruntime.Run(t.Context(), in)
	if err != nil || second.Status != sol.RunSuspended || second.SuspensionContext.ToolCallID != "second" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if e.calls != 1 || e.closes != 1 || len(model.DoStreamCalls) != 0 || len(second.Messages) == 0 || len(second.NewMessages) != 1 {
		t.Fatalf("execute=%d close=%d model=%d result=%+v", e.calls, e.closes, len(model.DoStreamCalls), second)
	}
}

func TestRunValidationPreventsWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*chatruntime.Input)
	}{
		{"model", func(in *chatruntime.Input) { in.Model = nil }},
		{"limits", func(in *chatruntime.Input) { in.ModelLimits = session.ModelLimits{} }},
		{"store", func(in *chatruntime.Input) { in.SessionStore = nil }},
		{"backend", func(in *chatruntime.Input) { in.Backend = nil }},
		{"sink", func(in *chatruntime.Input) { in.Sink = nil }},
		{"budget", func(in *chatruntime.Input) { in.MaxSteps = 0 }},
		{"factory", func(in *chatruntime.Input) { in.ExecutorFactory = nil }},
		{"delegated checkpoint", func(in *chatruntime.Input) { in.Resume = &sol.SuspensionContext{Reason: "delegated"} }},
		{"missing decision", func(in *chatruntime.Input) { in.Resume = &sol.SuspensionContext{Reason: "permission"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: reply()})
			in := input(model)
			tc.change(&in)
			if _, err := chatruntime.Run(t.Context(), in); err == nil {
				t.Fatal("invalid input accepted")
			}
			if len(model.DoStreamCalls) != 0 {
				t.Fatal("model invoked before validation")
			}
		})
	}
}

func TestRunCanonicalDispatchAndAttachments(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "javascript", true: "direct"}[direct], func(t *testing.T) {
			def := capability.Definition{Path: capability.Local(capability.Tool, "", "lookup"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object"}`)}
			call := jsCall("lookup-call", false)
			if direct {
				call = testutil.MockToolCallResponse("lookup-call", def.Path.Direct(), map[string]any{"id": "abc"}, testutil.MockUsage(10, 10))
			}
			model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{call, reply()}})
			in := input(model)
			in.DirectTools = direct
			in.Capabilities = []capability.Definition{def}
			called := 0
			in.Backend = backendFunc(func(ctx context.Context, got chatruntime.Invocation) (tool.Result, error) {
				called++
				if got.CapabilityID != "tool//lookup" || got.ToolCallID != "lookup-call" || string(got.Input) != `{"id":"abc"}` {
					t.Errorf("invocation=%+v", got)
				}
				return tool.Result{Output: `{"ok":true}`, Attachments: []tool.Attachment{{Data: "s3ref:/tmp/image.png", MimeType: "image/png"}}}, nil
			})
			e := &executor{execute: func(ctx context.Context, _ string, i jsexec.Invoker) (jsexec.Result, error) {
				out, err := i.Invoke(ctx, def.Path.ID(), json.RawMessage(`[{"id":"abc"}]`))
				return jsexec.Result{Output: out}, err
			}}
			in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) {
				if direct {
					t.Error("direct mode allocated")
				}
				return e, nil
			}
			result, err := chatruntime.Run(t.Context(), in)
			if err != nil || result.Status != sol.RunCompleted || called != 1 {
				t.Fatalf("result=%+v calls=%d err=%v", result, called, err)
			}
			encoded, _ := json.Marshal(model.DoStreamCalls[1])
			if !strings.Contains(string(encoded), "s3ref:/tmp/image.png") {
				t.Fatalf("attachment absent from next model turn: %s", encoded)
			}
		})
	}
}

func TestRunClosesOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	in := input(testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: jsCall("cancel", false)}))
	e := &executor{execute: func(ctx context.Context, _ string, _ jsexec.Invoker) (jsexec.Result, error) {
		cancel()
		return jsexec.Result{}, ctx.Err()
	}}
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
	_, err := chatruntime.Run(ctx, in)
	if !errors.Is(err, context.Canceled) || e.closes != 1 {
		t.Fatalf("error=%v close=%d", err, e.closes)
	}
}

func TestRunKeepsLogsOnScriptError(t *testing.T) {
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{jsCall("failed", false), reply()}})
	in := input(model)
	e := &executor{execute: func(context.Context, string, jsexec.Invoker) (jsexec.Result, error) {
		return jsexec.Result{Undefined: true, Logs: []jsexec.Log{{Level: jsexec.LogWarn, Message: "before failure"}}}, &jsexec.Exception{Name: "Error", Message: "failed script"}
	}}
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
	if _, err := chatruntime.Run(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(model.DoStreamCalls[1])
	if !strings.Contains(string(encoded), "before failure") || !strings.Contains(string(encoded), "failed script") || e.closes != 1 {
		t.Fatalf("logs/error lost: %s close=%d", encoded, e.closes)
	}
}
