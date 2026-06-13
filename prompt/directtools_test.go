package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRender_DirectTools_Shape asserts that a DirectTools=true render
// drops the JS-flavoured scaffolding (run_js intro, TS manifest, JS
// environment, namespaced bindings) and keeps the universal sections
// (env block, file-storage path conventions, file-sharing options).
func TestRender_DirectTools_Shape(t *testing.T) {
	data := AgentData{
		AgentDashboardURL: "https://dash",
		AgentRouteURL:     "https://agent",
		DirectTools:       true,
		Date:              "2026-06-08",
		Platform:          "telegram",
		Tools: []ToolInfo{
			{Name: "lookup_order", Description: "Look up an order.", Access: "public"},
		},
		Connections: []ConnInfo{
			{Slug: "slack", Description: "Slack workspace", Access: "user"},
		},
		MCPServers: []MCPServerStatus{
			{Slug: "github", Name: "GitHub", Status: "connected", Access: "user"},
		},
		ExecEndpoints: []ExecEndpointInfo{
			{Slug: "ci", Description: "CI host", Access: "user"},
		},
		Topics: []TopicInfo{
			{Slug: "build_done", Description: "fires on CI build", Access: "user"},
		},
	}
	out, err := Render(data, "public")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	mustNotContain := []string{
		"run_js",
		"## JavaScript environment",
		"## Custom JavaScript functions",
		"## Service connections",
		"## MCP servers",
		"## Remote command endpoints",
		"## Notification topics",
		"declare function",
		"declare const",
		"declare type FilePath",
		"`var` for top-level names",
		"conn_slack.requestJSON",
		"mcp_github.search_repos",
		"exec_ci.run",
		"topic_build_done.subscribe()",
	}
	for _, s := range mustNotContain {
		if strings.Contains(out, s) {
			t.Errorf("direct-mode render must NOT contain %q\nfull output:\n%s", s, out)
		}
	}

	mustContain := []string{
		"<env>",
		"Date: 2026-06-08",
		"Platform: telegram",
		"## Files",                 // S3-like storage conventions
		"## File attachments",      // attachment conventions
		"## Sharing files",         // delivery options
		"Call tools to get real answers",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("direct-mode render is missing %q", s)
		}
	}
}

// TestRender_DirectTools_SiblingShort confirms that the sibling
// section in direct mode advertises promptAgent but skips the typed
// agent_{slug}.<tool>() namespace prose (that belongs to JS mode).
func TestRender_DirectTools_SiblingShort(t *testing.T) {
	data := AgentData{
		DirectTools: true,
		Date:        "2026-06-08",
		Siblings: []SiblingInfo{
			{Slug: "spotify", Name: "Spotify", Description: "playback", Tools: []ToolInfo{
				{Name: "play", Description: "Play a track.", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}},
		},
	}
	out, err := Render(data, "user")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "## Sibling agents") {
		t.Fatal("sibling section should appear in direct mode")
	}
	if !strings.Contains(out, "promptAgent") {
		t.Fatal("sibling section should mention promptAgent in direct mode")
	}
	if strings.Contains(out, "declare const agent_spotify") {
		t.Fatal("direct mode must not emit TS declarations for siblings")
	}
	if strings.Contains(out, "agent_{slug}` namespace") {
		t.Fatal("direct mode must not mention the JS agent_{slug} namespace")
	}
}

// TestRender_DirectToolsFalse_KeepsJSPath verifies the existing JS-path
// render is unchanged when DirectTools=false (regression guard for the
// {{ if not .DirectTools }} wrappers).
func TestRender_DirectToolsFalse_KeepsJSPath(t *testing.T) {
	data := AgentData{
		Date:        "2026-06-08",
		Connections: []ConnInfo{{Slug: "slack", Description: "Slack", Access: "user"}},
	}
	out, err := Render(data, "user")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "run_js") {
		t.Error("JS path render must still mention run_js")
	}
	if !strings.Contains(out, "## Service connections") {
		t.Error("JS path render must still include Service connections section")
	}
	if !strings.Contains(out, "## JavaScript environment") {
		t.Error("JS path render must still include JavaScript environment section")
	}
}
