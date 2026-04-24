package agentsdk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/airlockrun/goai/schema"
)

// ToolRender is the data RenderToolDecls consumes. Airlock builds this from
// the hydrated DB/sync payload; the agent builds it from registeredTool.
// Both paths go through the same renderer so the LLM sees one format.
type ToolRender struct {
	Name          string
	Description   string
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	InputExamples []json.RawMessage
}

// RenderToolDecls emits a TypeScript .d.ts-style block describing each tool.
// Output is suitable for direct inclusion in an LLM prompt.
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

	// JSDoc block: description + @example lines.
	b.WriteString("/**\n")
	for _, line := range strings.Split(strings.TrimSpace(t.Description), "\n") {
		b.WriteString(" * ")
		b.WriteString(line)
		b.WriteString("\n")
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

// renderRegisteredTools adapts []*registeredTool into []ToolRender for the
// renderer. Used by the agent-side path when we want to render locally.
func renderRegisteredTools(tools []*registeredTool) string {
	items := make([]ToolRender, 0, len(tools))
	for _, t := range tools {
		inRaw, _ := json.Marshal(t.InputSchema)
		outRaw, _ := json.Marshal(t.OutputSchema)
		items = append(items, ToolRender{
			Name:          t.Name,
			Description:   t.Description,
			InputSchema:   inRaw,
			OutputSchema:  outRaw,
			InputExamples: t.InputExamples,
		})
	}
	// Stable ordering by name.
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return RenderToolDecls(items)
}
