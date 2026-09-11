// Package tsrender produces TypeScript declarations for agent capabilities.
package tsrender

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Type renders raw JSON Schema, including externally supplied MCP schemas.
// Validation-only constraints (such as minimum and pattern) do not affect the
// TypeScript shape. References must be local and acyclic. Malformed type-bearing
// keywords and unsupported references return errors, never replacement schemas.
// An absent output schema is unknown, as is the unrestricted schema {}.
func Type(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "unknown", nil
	}
	if !json.Valid(raw) {
		return "", errors.New("invalid JSON schema")
	}
	root := raw
	active := map[string]bool{}
	var render func(json.RawMessage, int) (string, error)
	render = func(raw json.RawMessage, depth int) (string, error) {
		if depth > 64 {
			return "", errors.New("schema nesting exceeds 64")
		}
		switch strings.TrimSpace(string(raw)) {
		case "true":
			return "unknown", nil
		case "false":
			return "never", nil
		case "null", "":
			return "", errors.New("schema must be an object or boolean")
		}
		var s struct {
			Ref                  string                     `json:"$ref"`
			Type                 json.RawMessage            `json:"type"`
			Format               string                     `json:"format"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties json.RawMessage            `json:"additionalProperties"`
			Items                json.RawMessage            `json:"items"`
			PrefixItems          json.RawMessage            `json:"prefixItems"`
			AnyOf                []json.RawMessage          `json:"anyOf"`
			OneOf                []json.RawMessage          `json:"oneOf"`
			AllOf                []json.RawMessage          `json:"allOf"`
			Enum                 []json.RawMessage          `json:"enum"`
			Const                json.RawMessage            `json:"const"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("invalid schema: %w", err)
		}
		var keywords map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keywords); err != nil {
			return "", err
		}
		for _, key := range []string{"$ref", "type", "properties", "required", "anyOf", "oneOf", "allOf", "enum", "format"} {
			if string(keywords[key]) == "null" {
				return "", fmt.Errorf("%s must not be null", key)
			}
		}
		var terms []string
		if _, exists := keywords["$ref"]; exists {
			pointer, err := url.PathUnescape(strings.TrimPrefix(s.Ref, "#"))
			if err != nil || !strings.HasPrefix(s.Ref, "#") || pointer != "" && !strings.HasPrefix(pointer, "/") {
				return "", fmt.Errorf("unsupported $ref %q: require a local JSON pointer", s.Ref)
			}
			if active[pointer] {
				return "", fmt.Errorf("recursive $ref %q cannot be rendered inline", s.Ref)
			}
			target := root
			if pointer != "" {
				for _, token := range strings.Split(pointer[1:], "/") {
					var object map[string]json.RawMessage
					if json.Unmarshal(target, &object) == nil && object != nil {
						target = object[strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")]
					} else {
						var array []json.RawMessage
						i, err := strconv.Atoi(token)
						if json.Unmarshal(target, &array) != nil || err != nil || i < 0 || i >= len(array) {
							return "", fmt.Errorf("unresolved $ref %q", s.Ref)
						}
						target = array[i]
					}
				}
			}
			if target == nil {
				return "", fmt.Errorf("unresolved $ref %q", s.Ref)
			}
			active[pointer] = true
			t, err := render(target, depth+1)
			delete(active, pointer)
			if err != nil {
				return "", fmt.Errorf("$ref %q: %w", s.Ref, err)
			}
			terms = append(terms, t)
		}
		for _, group := range []struct {
			name, sep string
			variants  []json.RawMessage
		}{{"anyOf", " | ", s.AnyOf}, {"oneOf", " | ", s.OneOf}, {"allOf", " & ", s.AllOf}} {
			if _, exists := keywords[group.name]; !exists {
				continue
			}
			if len(group.variants) == 0 {
				return "", fmt.Errorf("%s must be nonempty", group.name)
			}
			var parts []string
			for i, variant := range group.variants {
				t, err := render(variant, depth+1)
				if err != nil {
					return "", fmt.Errorf("%s[%d]: %w", group.name, i, err)
				}
				parts = append(parts, t)
			}
			terms = append(terms, join(parts, group.sep))
		}
		if s.Const != nil {
			terms = append(terms, literal(s.Const))
		}
		if _, exists := keywords["enum"]; exists {
			if len(s.Enum) == 0 {
				return "", errors.New("enum must be nonempty")
			}
			var parts []string
			for _, value := range s.Enum {
				parts = append(parts, literal(value))
			}
			terms = append(terms, join(parts, " | "))
		}
		var types []string
		if s.Type != nil {
			var single string
			if err := json.Unmarshal(s.Type, &single); err == nil {
				types = []string{single}
			} else if err := json.Unmarshal(s.Type, &types); err != nil || len(types) == 0 {
				return "", errors.New("type must be a string or nonempty string array")
			}
		} else if s.Properties != nil || s.AdditionalProperties != nil {
			types = []string{"object"}
		} else if s.Items != nil || s.PrefixItems != nil {
			types = []string{"array"}
		}
		var shapes []string
		for _, typ := range types {
			switch typ {
			case "string":
				t := "string"
				if s.Format == "agent-file" {
					t = "FilePath"
				} else if s.Format == "agent-dir" {
					t = "DirPath"
				}
				shapes = append(shapes, t)
			case "number", "integer":
				shapes = append(shapes, "number")
			case "boolean", "null":
				shapes = append(shapes, typ)
			case "array":
				if s.PrefixItems != nil {
					return "", errors.New("prefixItems tuple schemas are not supported")
				}
				items := s.Items
				if items == nil {
					items = json.RawMessage(`true`)
				}
				t, err := render(items, depth+1)
				if err != nil {
					return "", fmt.Errorf("items: %w", err)
				}
				shapes = append(shapes, parenthesize(t)+"[]")
			case "object":
				required := map[string]bool{}
				for _, name := range s.Required {
					required[name] = true
				}
				var names []string
				for name := range s.Properties {
					names = append(names, name)
				}
				sort.Strings(names)
				var fields, propertyTypes []string
				for _, name := range names {
					t, err := render(s.Properties[name], depth+1)
					if err != nil {
						return "", fmt.Errorf("property %q: %w", name, err)
					}
					field := propertyName(name)
					if !required[name] {
						field += "?"
						propertyTypes = append(propertyTypes, "undefined")
					}
					propertyTypes = append(propertyTypes, t)
					field += ": " + t + ";"
					var prop struct{ Description string }
					if json.Unmarshal(s.Properties[name], &prop) == nil && prop.Description != "" {
						field += " // " + strings.Join(strings.Fields(prop.Description), " ")
					}
					fields = append(fields, field)
				}
				additional := s.AdditionalProperties
				if additional == nil {
					additional = json.RawMessage(`true`)
				}
				t, err := render(additional, depth+1)
				if err != nil {
					return "", fmt.Errorf("additionalProperties: %w", err)
				}
				if len(fields) == 0 {
					shapes = append(shapes, "Record<string, "+t+">")
				} else {
					if t != "never" {
						// TS index signatures cover named properties too, so the
						// index type must include them for mixed fixed/map shapes.
						fields = append(fields, "[key: string]: "+join(append([]string{t}, propertyTypes...), " | ")+";")
					}
					shapes = append(shapes, "{\n  "+strings.ReplaceAll(strings.Join(fields, "\n"), "\n", "\n  ")+"\n}")
				}
			default:
				return "", fmt.Errorf("invalid schema type %q", typ)
			}
		}
		if len(shapes) > 0 {
			terms = append(terms, join(shapes, " | "))
		}
		return join(terms, " & "), nil
	}
	return render(raw, 0)
}

func parenthesize(s string) string {
	if strings.Contains(s, " | ") || strings.Contains(s, " & ") {
		return "(" + s + ")"
	}
	return s
}

func join(parts []string, sep string) string {
	var unique []string
	seen := map[string]bool{}
	for _, part := range parts {
		if sep == " | " && part == "unknown" {
			return "unknown"
		}
		if sep == " & " && part == "unknown" || seen[part] {
			continue
		}
		seen[part] = true
		unique = append(unique, part)
	}
	if len(unique) == 0 {
		return "unknown"
	}
	if len(unique) == 1 {
		return unique[0]
	}
	if sep == " & " {
		for i := range unique {
			unique[i] = parenthesize(unique[i])
		}
	}
	return strings.Join(unique, sep)
}

func propertyName(name string) string {
	for i, c := range name {
		if !(c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			b, _ := json.Marshal(name)
			return string(b)
		}
	}
	if name == "" {
		return `""`
	}
	return name
}

func literal(raw json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		var fields []string
		for name, value := range object {
			fields = append(fields, propertyName(name)+": "+literal(value)+";")
		}
		sort.Strings(fields)
		return "{ " + strings.Join(fields, " ") + " }"
	}
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) == nil && array != nil {
		var items []string
		for _, value := range array {
			items = append(items, literal(value))
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return strings.TrimSpace(string(raw))
}
