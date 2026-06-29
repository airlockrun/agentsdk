package agentsdk

// These tests verify the integration between agentsdk's RegisterTool path
// (the goai tool.Typed[In, Out] builder API) and the shared TS renderer in
// agentsdk/tsrender. Pure renderer tests live in tsrender/render_test.go;
// here we exercise schema generation from real Go types.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/tsrender"
	"github.com/airlockrun/goai/tool"
)

// renderRegisteredTools is a test-only adapter that turns []*registeredTool
// into the form tsrender consumes. Lives here (not in the tsrender package)
// because *registeredTool is private to agentsdk.
func renderRegisteredTools(tools []*registeredTool) string {
	items := make([]tsrender.ToolRender, 0, len(tools))
	for _, t := range tools {
		examples := make([]json.RawMessage, len(t.InputExamples))
		for i, ex := range t.InputExamples {
			examples[i] = ex.Input
		}
		items = append(items, tsrender.ToolRender{
			Name:          t.Name,
			Description:   t.Description,
			InputSchema:   t.InputSchema,
			OutputSchema:  t.OutputSchema,
			InputExamples: examples,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return tsrender.RenderToolDecls(items)
}

// reg builds a *registeredTool from a typed tool definition, for renderer tests.
func reg[In, Out any](name, desc string, fn tool.TypedFunc[In, Out], examples ...In) *registeredTool {
	d := tool.Typed[In, Out](name).Description(desc).Execute(fn)
	for _, ex := range examples {
		d = d.InputExample(ex)
	}
	return &registeredTool{Tool: d.Build()}
}

func TestRenderToolDecls_Primitive(t *testing.T) {
	type in struct {
		Q string `json:"q"`
	}
	type out struct {
		Hit string `json:"hit"`
	}
	got := renderRegisteredTools([]*registeredTool{reg[in, out]("search", "Search for text.", func(ctx context.Context, v in) (out, error) { return out{}, nil })})
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
	got := renderRegisteredTools([]*registeredTool{reg[in, out]("filter", "Filter users by name.", func(ctx context.Context, v in) (out, error) { return out{}, nil })})
	if !strings.Contains(got, "names: string[];") {
		t.Errorf("array of strings not rendered:\n%s", got)
	}
	if !strings.Contains(got, "limit?: number;") {
		t.Errorf("optional int not rendered:\n%s", got)
	}
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
	got := renderRegisteredTools([]*registeredTool{reg[in, out]("paginate", "Paginate results.", func(ctx context.Context, v in) (out, error) { return out{}, nil })})
	if !strings.Contains(got, "string | null") {
		t.Errorf("nullable pointer not rendered as union:\n%s", got)
	}
}

func TestRenderToolDecls_MultipleTools(t *testing.T) {
	type in struct {
		A string `json:"a"`
	}
	type out struct{}
	t1 := reg[in, out]("tool_b", "second", func(_ context.Context, _ in) (out, error) { return out{}, nil })
	t2 := reg[in, out]("tool_a", "first", func(_ context.Context, _ in) (out, error) { return out{}, nil })
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
	got := renderRegisteredTools([]*registeredTool{reg[in, out]("ping", "Health check.", func(ctx context.Context, v in) (out, error) { return out{OK: true}, nil })})
	if !strings.Contains(got, "args: {}") {
		t.Errorf("empty input should render as {}:\n%s", got)
	}
}

func TestRenderToolDecls_Example(t *testing.T) {
	type in struct {
		Query string `json:"query"`
	}
	type out struct{}
	got := renderRegisteredTools([]*registeredTool{reg[in, out]("search", "Search.", func(ctx context.Context, v in) (out, error) { return out{}, nil }, in{Query: "daft punk"})})
	if !strings.Contains(got, `@example search({"query":"daft punk"})`) {
		t.Errorf("input example not rendered:\n%s", got)
	}
}
