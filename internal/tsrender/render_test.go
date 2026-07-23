package tsrender

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/internal/binding"
)

func TestRenderToolDecls_Empty(t *testing.T) {
	if got := RenderToolDecls(nil); got != "" {
		t.Errorf("empty tools: want empty string, got %q", got)
	}
}

func TestRenderToolDecls_FromRawJSONSchema(t *testing.T) {
	// Airlock-side call path: schemas are already JSON-encoded from DB.
	in := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"Search text"}},"required":["q"]}`)
	out := json.RawMessage(`{"type":"object","properties":{"total":{"type":"integer"}}}`)
	got := RenderToolDecls([]ToolRender{{
		Path:         binding.Local(binding.Tool, "", "search"),
		Name:         "search",
		Description:  "Search.",
		InputSchema:  in,
		OutputSchema: out,
	}})
	if !strings.Contains(got, "q: string;") {
		t.Errorf("required field should not have ?:\n%s", got)
	}
	if !strings.Contains(got, "// Search text") {
		t.Errorf("description should render as comment:\n%s", got)
	}
	if !strings.Contains(got, "total?: number;") {
		t.Errorf("integer should render as number:\n%s", got)
	}
}

// LLMHint, when set, surfaces in the JSDoc block under the description in
// `[brackets]`. This is the model-only steer that pairs with Description
// (which may also surface in member-facing UIs).
func TestRenderToolDecls_LLMHintInJSDoc(t *testing.T) {
	in := json.RawMessage(`{"type":"object"}`)
	out := json.RawMessage(`{"type":"object"}`)
	got := RenderToolDecls([]ToolRender{{
		Path:         binding.Local(binding.Tool, "", "search"),
		Name:         "search",
		Description:  "Search the web.",
		LLMHint:      "expensive; cache results before re-calling",
		InputSchema:  in,
		OutputSchema: out,
	}})
	if !strings.Contains(got, " * Search the web.") {
		t.Errorf("description should render in JSDoc:\n%s", got)
	}
	if !strings.Contains(got, " * [expensive; cache results before re-calling]") {
		t.Errorf("LLMHint should render below description in brackets:\n%s", got)
	}
}

// Without an LLMHint the JSDoc stays clean (no empty bracket line).
func TestRenderToolDecls_OmitsEmptyLLMHint(t *testing.T) {
	got := RenderToolDecls([]ToolRender{{
		Path:         binding.Local(binding.Tool, "", "search"),
		Name:         "search",
		Description:  "Search the web.",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if strings.Contains(got, "[]") {
		t.Errorf("missing LLMHint should not produce empty brackets:\n%s", got)
	}
}

func TestRenderNestedRoot_Empty(t *testing.T) {
	if got := RenderNestedRoot("mcp", nil); got != "" {
		t.Errorf("empty tools: want empty string, got %q", got)
	}
}

func TestRenderNestedRoot_Basic(t *testing.T) {
	path := binding.Local(binding.MCP, "github", "search_repos")
	got := RenderNestedRoot("mcp", []NamespaceRender{{Namespace: "github", Tools: []MCPToolRender{
		{
			Path:        path,
			Name:        "search_repos",
			Description: "Search GitHub repositories.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
		},
	}}})
	wants := []string{
		"declare const mcp: {",
		"github: {",
		"/** Search GitHub repositories. */",
		"search_repos(args: {",
		"query: string;",
		"limit?: number;",
		"}): unknown;",
		"};",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n---\n%s", w, got)
		}
	}
}

// RenderMCPNamespace must NOT prepend a prefix — the caller owns the
// full identifier. A sibling namespace declares `agent_<slug>`, which
// must match the JS binding installed by vm.go (regression: it used to
// emit `mcp_agent_<slug>`, so the LLM called an undefined symbol).
func TestRenderNestedRootAgent(t *testing.T) {
	got := RenderNestedRoot("agent", []NamespaceRender{{Namespace: "spotify", Tools: []MCPToolRender{
		{Path: binding.Local(binding.Agent, "spotify", "get_current_status"), Name: "get_current_status", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}})
	if !strings.Contains(got, "declare const agent: {") || !strings.Contains(got, "spotify: {") {
		t.Errorf("want nested agent.spotify declaration, got:\n%s", got)
	}
	if strings.Contains(got, "mcp") {
		t.Errorf("must not render an MCP root:\n%s", got)
	}
}

func TestRenderNestedRoot_SortsTools(t *testing.T) {
	// Input order is intentionally non-alphabetic.
	got := RenderNestedRoot("mcp", []NamespaceRender{{Namespace: "svc", Tools: []MCPToolRender{
		{Path: binding.Local(binding.MCP, "svc", "zeta"), Name: "zeta", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Path: binding.Local(binding.MCP, "svc", "alpha"), Name: "alpha", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Path: binding.Local(binding.MCP, "svc", "mu"), Name: "mu", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}})
	alphaIdx := strings.Index(got, "alpha(")
	muIdx := strings.Index(got, "mu(")
	zetaIdx := strings.Index(got, "zeta(")
	if alphaIdx < 0 || muIdx < 0 || zetaIdx < 0 {
		t.Fatalf("missing one of the tools:\n%s", got)
	}
	if !(alphaIdx < muIdx && muIdx < zetaIdx) {
		t.Errorf("tools not in sorted order: alpha=%d mu=%d zeta=%d\n%s", alphaIdx, muIdx, zetaIdx, got)
	}
}

func TestRenderNestedRoot_NoDescription(t *testing.T) {
	got := RenderNestedRoot("mcp", []NamespaceRender{{Namespace: "svc", Tools: []MCPToolRender{
		{Path: binding.Local(binding.MCP, "svc", "ping"), Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}})
	if strings.Contains(got, "/**") {
		t.Errorf("missing description should not produce JSDoc:\n%s", got)
	}
	if !strings.Contains(got, "ping(args: {}): unknown;") {
		t.Errorf("empty-object args should render as `{}`:\n%s", got)
	}
}
