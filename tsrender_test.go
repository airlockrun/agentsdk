package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderToolDecls_Empty(t *testing.T) {
	if got := RenderToolDecls(nil); got != "" {
		t.Errorf("empty tools: want empty string, got %q", got)
	}
}

func TestRenderToolDecls_Primitive(t *testing.T) {
	type in struct {
		Q string `json:"q"`
	}
	type out struct {
		Hit string `json:"hit"`
	}
	tool := &Tool[in, out]{
		Name:        "search",
		Description: "Search for text.",
		Execute:     func(ctx context.Context, v in) (out, error) { return out{}, nil },
	}
	got := renderRegisteredTools([]*registeredTool{tool.toRegistered()})
	want := []string{
		"/**\n * Search for text.\n */",
		"declare function search(args: {",
		"q: string;",
		"}): {",
		"hit: string;",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n---\n%s", w, got)
		}
	}
}

func TestRenderToolDecls_ArrayAndOptional(t *testing.T) {
	type in struct {
		Names []string `json:"names"`
		Limit int      `json:"limit,omitempty"`
	}
	type out struct {
		Matches []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"matches"`
	}
	tool := &Tool[in, out]{
		Name:        "filter",
		Description: "Filter users by name.",
		Execute:     func(ctx context.Context, v in) (out, error) { return out{}, nil },
	}
	got := renderRegisteredTools([]*registeredTool{tool.toRegistered()})
	// Array rendering (required).
	if !strings.Contains(got, "names: string[];") {
		t.Errorf("array of strings not rendered:\n%s", got)
	}
	// Optional with omitempty.
	if !strings.Contains(got, "limit?: number;") {
		t.Errorf("optional int not rendered:\n%s", got)
	}
	// Nested struct array.
	if !strings.Contains(got, "matches: {") {
		t.Errorf("nested array not rendered:\n%s", got)
	}
}

func TestRenderToolDecls_NullablePointer(t *testing.T) {
	type in struct {
		Cursor *string `json:"cursor"`
	}
	type out struct {
		Next *string `json:"next"`
	}
	tool := &Tool[in, out]{
		Name:        "paginate",
		Description: "Paginate results.",
		Execute:     func(ctx context.Context, v in) (out, error) { return out{}, nil },
	}
	got := renderRegisteredTools([]*registeredTool{tool.toRegistered()})
	if !strings.Contains(got, "string | null") {
		t.Errorf("nullable pointer not rendered as union:\n%s", got)
	}
}

func TestRenderToolDecls_MultipleTools(t *testing.T) {
	type in struct {
		A string `json:"a"`
	}
	type out struct{}
	t1 := (&Tool[in, out]{Name: "tool_b", Description: "second", Execute: func(_ context.Context, _ in) (out, error) { return out{}, nil }}).toRegistered()
	t2 := (&Tool[in, out]{Name: "tool_a", Description: "first", Execute: func(_ context.Context, _ in) (out, error) { return out{}, nil }}).toRegistered()
	got := renderRegisteredTools([]*registeredTool{t1, t2})

	aIdx := strings.Index(got, "tool_a")
	bIdx := strings.Index(got, "tool_b")
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("both tool names must appear:\n%s", got)
	}
	if aIdx > bIdx {
		t.Errorf("tools should be sorted by name; got tool_b before tool_a:\n%s", got)
	}
}

func TestRenderToolDecls_EmptyObject(t *testing.T) {
	type in struct{}
	type out struct {
		OK bool `json:"ok"`
	}
	tool := &Tool[in, out]{
		Name:        "ping",
		Description: "Health check.",
		Execute:     func(ctx context.Context, v in) (out, error) { return out{OK: true}, nil },
	}
	got := renderRegisteredTools([]*registeredTool{tool.toRegistered()})
	if !strings.Contains(got, "args: {}") {
		t.Errorf("empty input should render as {}:\n%s", got)
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

func TestRenderToolDecls_Example(t *testing.T) {
	type in struct {
		Query string `json:"query"`
	}
	type out struct{}
	tool := &Tool[in, out]{
		Name:          "search",
		Description:   "Search.",
		Execute:       func(ctx context.Context, v in) (out, error) { return out{}, nil },
		InputExamples: []in{{Query: "daft punk"}},
	}
	got := renderRegisteredTools([]*registeredTool{tool.toRegistered()})
	if !strings.Contains(got, `@example search({"query":"daft punk"})`) {
		t.Errorf("input example not rendered:\n%s", got)
	}
}
