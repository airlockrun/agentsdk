package binding

import (
	"regexp"
	"strings"
	"testing"
)

func TestLocalPaths(t *testing.T) {
	tests := []struct {
		path       Path
		wantJS     string
		wantDirect string
	}{
		{Local(Air, "", "file_read"), "air.fileRead", "air__file_read"},
		{Local(Air, "", "http_request"), "air.httpRequest", "air__http_request"},
		{Local(Tool, "", "calculate"), "tools.calculate", "tool__calculate"},
		{Local(Connection, "gmail", "request_json"), "conn.gmail.requestJSON", "conn__gmail__request_json"},
		{Local(Exec, "ci_runner", "run"), "exec.ci_runner.run", "exec__ci_runner__run"},
		{Local(Topic, "alerts", "subscribe"), "topic.alerts.subscribe", "topic__alerts__subscribe"},
		{Local(MCP, "github", "search_issues"), "mcp.github.search_issues", "mcp__github__search_issues"},
		{Local(Agent, "sales_app", "create_lead"), "agent.sales_app.create_lead", "agent__sales_app__create_lead"},
		{AgentPrompt(), "", "agent__prompt"},
	}
	for _, tt := range tests {
		if got := tt.path.JS(); got != tt.wantJS {
			t.Errorf("JS() = %q, want %q", got, tt.wantJS)
		}
		if got := tt.path.Direct(); got != tt.wantDirect {
			t.Errorf("Direct() = %q, want %q", got, tt.wantDirect)
		}
	}
}

func TestExternalNormalizationCollisions(t *testing.T) {
	names := []string{"foo-bar", "foo_bar", "123 lookup", "search/issues", "💥"}
	paths, err := External(MCP, "github", "github", names)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths["123 lookup"].JS(); got != "mcp.github.tool_123_lookup" {
		t.Fatalf("digit-leading alias = %q", got)
	}
	if got := paths["search/issues"].JS(); got != "mcp.github.search_issues" {
		t.Fatalf("slash alias = %q", got)
	}
	if got := paths["💥"].JS(); got != "mcp.github.tool" {
		t.Fatalf("empty normalized alias = %q", got)
	}
	first := paths["foo-bar"].JS()
	second := paths["foo_bar"].JS()
	if first == second || !strings.Contains(first, "foo_bar_h") || !strings.Contains(second, "foo_bar_h") {
		t.Fatalf("collision aliases = %q, %q", first, second)
	}

	reversed, err := External(MCP, "github", "github", []string{"foo_bar", "foo-bar"})
	if err != nil {
		t.Fatal(err)
	}
	if reversed["foo-bar"].JS() != first || reversed["foo_bar"].JS() != second {
		t.Fatal("aliases depend on input order")
	}
}

func TestExternalRejectsDuplicateCanonicalName(t *testing.T) {
	if _, err := External(MCP, "github", "github", []string{"search", "search"}); err == nil {
		t.Fatal("duplicate canonical operation accepted")
	}
}

func TestDirectNameLength(t *testing.T) {
	path := Local(MCP, strings.Repeat("a", 44), strings.Repeat("b", 80))
	name := path.Direct()
	if len(name) != MaxDirectNameLength {
		t.Fatalf("len(Direct()) = %d, want %d", len(name), MaxDirectNameLength)
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(name) {
		t.Fatalf("invalid direct name %q", name)
	}
	if path.Direct() != name {
		t.Fatal("direct truncation is unstable")
	}
	other := Local(MCP, strings.Repeat("a", 44), strings.Repeat("b", 79)+"c").Direct()
	if other == name {
		t.Fatal("distinct canonical paths collided")
	}
}

func TestSiblingNamespace(t *testing.T) {
	if got := SiblingNamespace("sales-app"); got != "sales_app" {
		t.Fatalf("SiblingNamespace = %q", got)
	}
	if got := SiblingNamespace("123-app"); got != "_123_app" {
		t.Fatalf("digit-leading SiblingNamespace = %q", got)
	}
}
