// Package tsrender produces the TypeScript .d.ts blocks that Airlock
// embeds into the agent system prompt: typed signatures for registered
// tools, and per-server `declare const mcp_{slug}: {...}` namespaces for
// MCP tools.
//
// The package is split out from agentsdk's main API surface so builders
// (agentsdk consumers) don't see these types in their import autocomplete
// or godoc — agentsdk's main package stays focused on the builder API
// (RegisterTool, MCPHandle, etc.). The rendering here is shared between
// agentsdk's tests and the airlock module's prompt template; both import
// this package directly.
package tsrender

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/airlockrun/goai/schema"
)

// ToolRender is the data RenderToolDecls consumes. Airlock builds this
// from the hydrated DB/sync payload; the agent assembles it from the
// registered-tool schemas in tests. Both paths go through the same
// renderer so the LLM sees one format.
//
// LLMHint is optional model-only guidance that pairs with Description
// (which may also surface in member-facing UIs). When non-empty it's
// appended to the JSDoc block in `[brackets]` so the LLM gets the
// extra steer without polluting the user-visible description.
type ToolRender struct {
	Name          string
	Description   string
	LLMHint       string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	InputExamples []json.RawMessage
}

// RenderToolDecls emits a TypeScript .d.ts-style block describing each
// tool. Output is suitable for direct inclusion in an LLM prompt.
func RenderToolDecls(tools []ToolRender) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range tools {
		if i > 0 {
			b.WriteString("\n")
		}
		renderToolDecl(&b, t)
	}
	return b.String()
}

func renderToolDecl(b *strings.Builder, t ToolRender) {
	inSchema := decodeSchema(t.InputSchema)
	outSchema := decodeSchema(t.OutputSchema)

	// JSDoc block: description (+ optional LLMHint in brackets) + @example lines.
	b.WriteString("/**\n")
	for _, line := range strings.Split(strings.TrimSpace(t.Description), "\n") {
		b.WriteString(" * ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if hint := strings.TrimSpace(t.LLMHint); hint != "" {
		b.WriteString(" * [")
		b.WriteString(hint)
		b.WriteString("]\n")
	}
	for _, ex := range t.InputExamples {
		b.WriteString(" * @example ")
		b.WriteString(t.Name)
		b.WriteString("(")
		b.Write(ex)
		b.WriteString(")\n")
	}
	b.WriteString(" */\n")

	b.WriteString("declare function ")
	b.WriteString(t.Name)
	b.WriteString("(args: ")
	b.WriteString(tsTypeFromSchema(inSchema, 0))
	b.WriteString("): ")
	b.WriteString(tsTypeFromSchema(outSchema, 0))
	b.WriteString(";\n")
}

func decodeSchema(raw json.RawMessage) *schema.Schema {
	if len(raw) == 0 {
		return &schema.Schema{}
	}
	var s schema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return &schema.Schema{}
	}
	return &s
}

// tsTypeFromSchema renders a TypeScript type literal for a schema.
// indent is the current indentation depth (0 = top-level).
func tsTypeFromSchema(s *schema.Schema, indent int) string {
	if s == nil {
		return "any"
	}

	// Nullable: goai emits {anyOf: [T, {type: "null"}]} for pointer / nullable fields.
	if len(s.AnyOf) == 2 {
		a, b := s.AnyOf[0], s.AnyOf[1]
		if b != nil && b.Type == "null" {
			return tsTypeFromSchema(a, indent) + " | null"
		}
		if a != nil && a.Type == "null" {
			return tsTypeFromSchema(b, indent) + " | null"
		}
	}

	// Const → literal type.
	if s.Const != nil {
		return literalType(s.Const)
	}

	// Enum → union of literals.
	if len(s.Enum) > 0 {
		parts := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			parts = append(parts, literalType(v))
		}
		return strings.Join(parts, " | ")
	}

	switch s.Type {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		if s.Items == nil {
			return "any[]"
		}
		inner := tsTypeFromSchema(s.Items, indent)
		// Parenthesize unions inside arrays for readability.
		if strings.Contains(inner, " | ") {
			return "(" + inner + ")[]"
		}
		return inner + "[]"
	case "object", "":
		return renderObjectType(s, indent)
	}

	return "any"
}

func renderObjectType(s *schema.Schema, indent int) string {
	if len(s.Properties) == 0 {
		// Empty object (no-arg tool input, or untyped output).
		return "{}"
	}

	requiredSet := make(map[string]bool, len(s.Required))
	for _, name := range s.Required {
		requiredSet[name] = true
	}

	// Sort property names for stable output.
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	pad := strings.Repeat("  ", indent+1)
	closePad := strings.Repeat("  ", indent)

	var b strings.Builder
	b.WriteString("{\n")
	for _, name := range names {
		prop := s.Properties[name]
		b.WriteString(pad)
		b.WriteString(name)
		if !requiredSet[name] {
			b.WriteString("?")
		}
		b.WriteString(": ")
		b.WriteString(tsTypeFromSchema(prop, indent+1))
		b.WriteString(";")
		if prop != nil && prop.Description != "" {
			b.WriteString(" // ")
			// Single-line: collapse any embedded newlines.
			b.WriteString(strings.ReplaceAll(prop.Description, "\n", " "))
		}
		b.WriteString("\n")
	}
	b.WriteString(closePad)
	b.WriteString("}")
	return b.String()
}

// literalType renders a JSON value as a TypeScript literal type.
func literalType(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers come through as float64 after Unmarshal; format cleanly.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case nil:
		return "null"
	}
	return "any"
}

// MCPToolRender carries the bits Airlock has cached about an MCP tool.
// Only the input shape is typed — MCP doesn't define an output schema, so
// the rendered return type is always `unknown` (caller does runtime parsing).
type MCPToolRender struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// JSToolNames maps each MCP tool name to a JS-identifier-safe property
// name. MCP tool names commonly include hyphens (`notion-update-page`);
// JS parses `obj.notion-update-page(...)` as arithmetic and throws
// ReferenceError. The original hyphenated name stays canonical on the
// wire (tool/call JSON-RPC); only the JS surface is renamed.
//
// Collision handling: when the `-` → `_` rename would clash with
// another tool's original name on the same server (e.g. `foo-bar` AND
// `foo_bar` both exist), the hyphenated tool keeps its original name —
// the LLM has to use bracket notation for that one, but every other
// tool on the server still gets the dot-friendly form. Iteration is
// over a sorted copy so the resulting map is stable across syncs.
func JSToolNames(names []string) map[string]string {
	taken := make(map[string]bool, len(names))
	for _, n := range names {
		taken[n] = true
	}
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)
	out := make(map[string]string, len(sorted))
	for _, n := range sorted {
		renamed := strings.ReplaceAll(n, "-", "_")
		if renamed == n || taken[renamed] {
			out[n] = n
			continue
		}
		taken[renamed] = true
		out[n] = renamed
	}
	return out
}

// RenderMCPNamespace emits a typed `declare const mcp_{slug}: { ... };`
// block describing each discovered MCP tool as a method on the namespace
// object. Mirrors the JS binding shape installed by agentsdk's vm.go so
// the LLM's call site and the runtime stay in lockstep.
//
//	declare const mcp_github: {
//	  /** Search for GitHub repositories. */
//	  search_repos(args: { query: string }): unknown;
//	  ...
//	};
func RenderMCPNamespace(slug string, tools []MCPToolRender) string {
	if len(tools) == 0 {
		return ""
	}
	sorted := make([]MCPToolRender, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	names := make([]string, len(sorted))
	for i, t := range sorted {
		names[i] = t.Name
	}
	jsNames := JSToolNames(names)

	var b strings.Builder
	b.WriteString("declare const mcp_")
	b.WriteString(slug)
	b.WriteString(": {\n")
	for _, t := range sorted {
		if desc := strings.TrimSpace(t.Description); desc != "" {
			b.WriteString("  /** ")
			b.WriteString(strings.ReplaceAll(desc, "\n", " "))
			b.WriteString(" */\n")
		}
		jsName := jsNames[t.Name]
		// Quote when the name still has illegal-identifier chars (the
		// collision fallback path). TS object literal type syntax accepts
		// quoted property names; keeps the declaration valid even though
		// the LLM will need bracket notation to call it.
		b.WriteString("  ")
		if jsName == t.Name && strings.ContainsAny(jsName, "-") {
			b.WriteString(`"`)
			b.WriteString(jsName)
			b.WriteString(`"`)
		} else {
			b.WriteString(jsName)
		}
		b.WriteString("(args: ")
		b.WriteString(tsTypeFromSchema(decodeSchema(t.InputSchema), 1))
		b.WriteString("): unknown;\n")
	}
	b.WriteString("};\n")
	return b.String()
}
