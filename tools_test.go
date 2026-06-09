package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/goai/tool"
)

// TestRunJSAutoConfirmSkipsGate verifies that an autoConfirm run executes
// run_js code that asked for request_confirmation without ever consulting
// the permission manager. No PermissionManager is placed in ctx — if the
// confirmation gate were reached, pm.Ask would panic or suspend, so a
// clean result here proves the gate was skipped.
func TestRunJSAutoConfirmSkipsGate(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "run-ac", "", "", context.Background())
	run.autoConfirm = true

	rjt := buildRunJSTool(a, run)
	res, err := rjt.Execute(context.Background(),
		json.RawMessage(`{"code":"6*7","request_confirmation":true}`),
		tool.CallOptions{ToolCallID: "tc-1"})
	if err != nil {
		t.Fatalf("autoConfirm run_js should execute, got error: %v", err)
	}
	if !strings.Contains(res.Output, "42") {
		t.Fatalf("expected result 42 in output, got %q", res.Output)
	}
}

// The rendered system prompt carries the built-in bindings reference and
// the admin-gated bindings. Registered tools are rendered separately by
// renderTools (TS declare blocks); they must not appear in the
// "## Built-in functions" section.
func TestSystemPrompt_BuiltinsAndAdminGate(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "check_gmail",
		Description: "Search Gmail inbox.",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})

	admin := a.renderSystemPrompt(AccessAdmin, nil, nil, promptEnv{Date: "2026-06-09"}, false)
	if !strings.Contains(admin, "## Built-in functions") {
		t.Fatal("admin prompt should include the Built-in functions section")
	}
	if !strings.Contains(admin, "fileRead(path)") {
		t.Fatal("admin prompt should list fileRead")
	}
	if !strings.Contains(admin, "queryDB(sql") {
		t.Fatal("admin prompt should advertise queryDB")
	}

	user := a.renderSystemPrompt(AccessUser, nil, nil, promptEnv{Date: "2026-06-09"}, false)
	if strings.Contains(user, "queryDB(sql") || strings.Contains(user, "execDB(sql") {
		t.Fatal("AccessUser prompt must not advertise queryDB/execDB")
	}
}

// Topic LLMHint surfaces in the topic inventory line of the rendered
// system prompt, in brackets after the description.
func TestSystemPrompt_TopicIncludesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTopic(&Topic{
		Slug:        "build_done",
		Description: "fires when a CI build completes",
		LLMHint:     "subscribe only when the user explicitly opts in",
	})

	prompt := a.renderSystemPrompt(AccessUser, nil, nil, promptEnv{Date: "2026-06-09"}, false)
	if !strings.Contains(prompt, "topic_build_done") {
		t.Errorf("expected topic binding in inventory; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[subscribe only when the user explicitly opts in]") {
		t.Errorf("expected topic LLMHint in inventory; got:\n%s", prompt)
	}
}

// agentsdk.Tool.LLMHint flows through into registeredTool — the tsrender
// path picks it up from there. Verifies the field actually persists past
// toRegistered() rather than being dropped.
// Tool errors must surface with the tool name prefixed so the LLM /
// operator can tell which tool failed from the JS stack trace alone.
// Without the prefix, stdlib errors like "unexpected end of JSON input"
// point only at agentsdk.newVM.func1 (the dispatch closure) and the
// reader has no way to know which tool's Go code produced them.
func TestRegisterTool_WrapsExecuteErrorWithName(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "broken_tool",
		Description: "always errors",
		Execute: func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{}, errors.New("unexpected end of JSON input")
		},
	})
	rt := a.tools["broken_tool"]
	_, err := rt.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "broken_tool") {
		t.Errorf("error %q should include tool name", err.Error())
	}
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error %q should preserve underlying message", err.Error())
	}
}

func TestRegisterTool_PreservesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "search",
		Description: "Search the web.",
		LLMHint:     "expensive; cache results before re-calling",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})
	rt, ok := a.tools["search"]
	if !ok {
		t.Fatal("expected tool 'search' to be registered")
	}
	if rt.LLMHint != "expensive; cache results before re-calling" {
		t.Errorf("registeredTool.LLMHint = %q, want hint preserved", rt.LLMHint)
	}
}

// LLMHint is the way to steer the model away from a directory while
// keeping it reachable in code. Authorization stays with Read/Write/List
// — admins can still reach the directory; the hint is purely model-
// facing guidance surfaced alongside the directory's caps in the system
// prompt.
func TestSystemPrompt_DirectoryIncludesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("cache", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser,
		Description: "builder-managed cache",
		LLMHint:     "internal cache; do not list or modify",
	})

	prompt := a.renderSystemPrompt(AccessAdmin, nil, nil, promptEnv{Date: "2026-06-09"}, false)
	if !strings.Contains(prompt, "cache/") {
		t.Errorf("expected cache path in inventory; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "builder-managed cache") {
		t.Errorf("expected description in inventory; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[internal cache; do not list or modify]") {
		t.Errorf("expected LLMHint in inventory; got:\n%s", prompt)
	}
}

// Declarations are rendered by the shared RenderToolDecls helper; verify
// the expected TypeScript surface from a list of registered tools.
func TestRegisteredToolsRenderToDecls(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "greet",
		Description: "Say hi.",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})
	tools := make([]*registeredTool, 0, len(a.tools))
	for _, tt := range a.tools {
		tools = append(tools, tt)
	}
	got := renderRegisteredTools(tools)
	if !strings.Contains(got, "declare function greet(args: {") {
		t.Fatalf("declaration not rendered:\n%s", got)
	}
}
