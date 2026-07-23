// Package tsrender produces TypeScript declarations for agent capabilities.
package tsrender

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/airlockrun/agentsdk/internal/binding"
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
	Path          binding.Path
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
	b.WriteString("declare const tools: {\n")
	for i, t := range tools {
		if i > 0 {
			b.WriteString("\n")
		}
		renderToolMethod(&b, t, "  ")
	}
	b.WriteString("};\n")
	return b.String()
}

func renderToolMethod(b *strings.Builder, t ToolRender, indent string) {
	inSchema := decodeSchema(t.InputSchema)
	outSchema := decodeSchema(t.OutputSchema)
	parts := t.Path.JSParts()
	if len(parts) == 0 {
		panic("tsrender: tool binding path is required")
	}
	name := parts[len(parts)-1]

	// JSDoc block: description (+ optional LLMHint in brackets) + @example lines.
	b.WriteString(indent)
	b.WriteString("/**\n")
	for _, line := range strings.Split(strings.TrimSpace(t.Description), "\n") {
		b.WriteString(indent)
		b.WriteString(" * ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if hint := strings.TrimSpace(t.LLMHint); hint != "" {
		b.WriteString(indent)
		b.WriteString(" * [")
		b.WriteString(hint)
		b.WriteString("]\n")
	}
	for _, ex := range t.InputExamples {
		b.WriteString(indent)
		b.WriteString(" * @example tools.")
		b.WriteString(name)
		b.WriteString("(")
		b.Write(ex)
		b.WriteString(")\n")
	}
	b.WriteString(indent)
	b.WriteString(" */\n")

	b.WriteString(indent)
	b.WriteString(name)
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
		// agentsdk.FilePath / DirPath travel as `format` markers on the
		// JSON Schema. Render them as TS type aliases so the LLM sees
		// the semantic, not just `string`. The aliases are `declare`d
		// once in the prompt header.
		switch s.Format {
		case "agent-file":
			return "FilePath"
		case "agent-dir":
			return "DirPath"
		}
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
	Path        binding.Path
	Name        string
	Description string
	InputSchema json.RawMessage
}

// NamespaceRender describes one namespace beneath an external capability root.
type NamespaceRender struct {
	Namespace string
	Tools     []MCPToolRender
}

// RenderNestedRoot emits one declaration for a nested MCP or agent root.
func RenderNestedRoot(root string, namespaces []NamespaceRender) string {
	if len(namespaces) == 0 {
		return ""
	}
	sortedNamespaces := make([]NamespaceRender, len(namespaces))
	copy(sortedNamespaces, namespaces)
	sort.Slice(sortedNamespaces, func(i, j int) bool { return sortedNamespaces[i].Namespace < sortedNamespaces[j].Namespace })

	var b strings.Builder
	b.WriteString("declare const ")
	b.WriteString(root)
	b.WriteString(": {\n")
	for _, namespace := range sortedNamespaces {
		b.WriteString("  ")
		b.WriteString(namespace.Namespace)
		b.WriteString(": {\n")
		tools := make([]MCPToolRender, len(namespace.Tools))
		copy(tools, namespace.Tools)
		sort.Slice(tools, func(i, j int) bool { return tools[i].Path.JS() < tools[j].Path.JS() })
		for _, t := range tools {
			parts := t.Path.JSParts()
			if len(parts) == 0 {
				panic("tsrender: external binding path is required")
			}
			if desc := strings.TrimSpace(t.Description); desc != "" {
				b.WriteString("    /** ")
				b.WriteString(strings.ReplaceAll(desc, "\n", " "))
				b.WriteString(" */\n")
			}
			b.WriteString("    ")
			b.WriteString(parts[len(parts)-1])
			b.WriteString("(args: ")
			b.WriteString(tsTypeFromSchema(decodeSchema(t.InputSchema), 2))
			b.WriteString("): unknown;\n")
		}
		b.WriteString("  };\n")
	}
	b.WriteString("};\n")
	return b.String()
}
