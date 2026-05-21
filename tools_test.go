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

// buildToolDescription now covers run_js usage + built-in bindings only.
// Registered tools are rendered by Airlock into the system prompt (via
// RenderToolDecls), so they must NOT appear in the run_js description.
func TestToolDescription(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "check_gmail",
		Description: "Search Gmail inbox.",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "send_slack",
		Description: "Post to Slack.",
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})

	desc := buildToolDescription(a, AccessAdmin)
	if !strings.Contains(desc, "conn_{slug}.request(") {
		t.Fatal("expected built-in bindings in description")
	}
	if strings.Contains(desc, "check_gmail") || strings.Contains(desc, "send_slack") {
		t.Fatal("tool names should not appear in run_js description — Airlock renders them")
	}
	if !strings.Contains(desc, "queryDB(") {
		t.Fatal("admin description should include queryDB")
	}

	// Non-admin callers must not see queryDB / execDB advertised — they
	// aren't bound in the VM either, so leaving them in the prompt would
	// just invite the LLM to fail.
	userDesc := buildToolDescription(a, AccessUser)
	if strings.Contains(userDesc, "queryDB(") || strings.Contains(userDesc, "execDB(") {
		t.Fatal("AccessUser description must not advertise queryDB/execDB")
	}
}

// Topic LLMHint surfaces in the topic inventory line, in brackets after
// the description, mirroring how Directory.LLMHint is rendered.
func TestTopicInventory_IncludesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTopic(&Topic{
		Slug:        "build_done",
		Description: "fires when a CI build completes",
		LLMHint:     "subscribe only when the user explicitly opts in",
	})

	desc := buildToolDescription(a, AccessUser)
	if !strings.Contains(desc, "topic_build_done.subscribe()") {
		t.Errorf("expected topic binding in inventory; got:\n%s", desc)
	}
	if !strings.Contains(desc, "[subscribe only when the user explicitly opts in]") {
		t.Errorf("expected topic LLMHint in inventory; got:\n%s", desc)
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
func TestDirectoryInventory_IncludesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("cache", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser,
		Description: "builder-managed cache",
		LLMHint:     "internal cache; do not list or modify",
	})

	desc := buildToolDescription(a, AccessAdmin)
	if !strings.Contains(desc, "cache (read+write+list)") {
		t.Errorf("expected cache caps in inventory; got:\n%s", desc)
	}
	if !strings.Contains(desc, "builder-managed cache") {
		t.Errorf("expected description in inventory; got:\n%s", desc)
	}
	if !strings.Contains(desc, "[internal cache; do not list or modify]") {
		t.Errorf("expected LLMHint in inventory; got:\n%s", desc)
	}
}

// Without an LLMHint the inventory line stays clean (no trailing brackets).
func TestDirectoryInventory_OmitsEmptyLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("uploads", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser,
		Description: "user uploads",
	})

	desc := buildToolDescription(a, AccessUser)
	if !strings.Contains(desc, "- uploads (read+write+list) — user uploads\n") {
		t.Errorf("expected clean inventory line without trailing brackets; got:\n%s", desc)
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
