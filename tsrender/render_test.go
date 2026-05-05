package tsrender

import (
	"encoding/json"
	"strings"
	"testing"
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
		Name:         "search",
		Description:  "Search the web.",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if strings.Contains(got, "[]") {
		t.Errorf("missing LLMHint should not produce empty brackets:\n%s", got)
	}
}

func TestRenderMCPNamespace_Empty(t *testing.T) {
	if got := RenderMCPNamespace("github", nil); got != "" {
		t.Errorf("empty tools: want empty string, got %q", got)
	}
}

func TestRenderMCPNamespace_Basic(t *testing.T) {
	got := RenderMCPNamespace("github", []MCPToolRender{
		{
			Name:        "search_repos",
			Description: "Search GitHub repositories.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
		},
	})
	wants := []string{
		"declare const mcp_github: {",
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

func TestRenderMCPNamespace_SortsTools(t *testing.T) {
	// Input order is intentionally non-alphabetic.
	got := RenderMCPNamespace("svc", []MCPToolRender{
		{Name: "zeta", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "alpha", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "mu", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
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

func TestRenderMCPNamespace_NoDescription(t *testing.T) {
	got := RenderMCPNamespace("svc", []MCPToolRender{
		{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	if strings.Contains(got, "/**") {
		t.Errorf("missing description should not produce JSDoc:\n%s", got)
	}
	if !strings.Contains(got, "ping(args: {}): unknown;") {
		t.Errorf("empty-object args should render as `{}`:\n%s", got)
	}
}
