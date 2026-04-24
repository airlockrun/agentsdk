package agentsdk

import (
	"context"
	"strings"
	"testing"
)

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

	desc := buildToolDescription(a)
	if !strings.Contains(desc, "conn_{slug}.request(") {
		t.Fatal("expected built-in bindings in description")
	}
	if strings.Contains(desc, "check_gmail") || strings.Contains(desc, "send_slack") {
		t.Fatal("tool names should not appear in run_js description — Airlock renders them")
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
