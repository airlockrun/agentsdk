package tsrender

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestType(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"absent output", "", "unknown"},
		{"unrestricted", `{}`, "unknown"},
		{"true", `true`, "unknown"},
		{"false", `false`, "never"},
		{"type array", `{"type":["string","null"]}`, "string | null"},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"number"},{"type":"null"}]}`, "string | number | null"},
		{"oneOf", `{"oneOf":[{"type":"boolean"},{"type":"integer"}]}`, "boolean | number"},
		{"allOf", `{"allOf":[{"anyOf":[{"type":"string"},{"type":"null"}]},{"const":"yes"}]}`, `(string | null) & "yes"`},
		{"union array", `{"type":"array","items":{"type":["string","null"]}}`, "(string | null)[]"},
		{"untyped array", `{"type":"array"}`, "unknown[]"},
		{"typed map", `{"type":"object","additionalProperties":{"type":"integer"}}`, "Record<string, number>"},
		{"nested map", `{"type":"object","additionalProperties":{"type":"object","additionalProperties":{"type":["string","null"]}}}`, "Record<string, Record<string, string | null>>"},
		{"open object", `{"type":"object","additionalProperties":true}`, "Record<string, unknown>"},
		{"closed empty object", `{"type":"object","additionalProperties":false}`, "Record<string, never>"},
		{"reference", `{"$defs":{"Result":{"type":"array","items":{"type":"string"}}},"$ref":"#/$defs/Result"}`, "string[]"},
		{"reference exact integer", `{"$defs":{"ID":{"const":9007199254740993}},"$ref":"#/$defs/ID"}`, "9007199254740993"},
		{"reference array pointer", `{"$defs":{"Result":{"anyOf":[{"type":"string"},{"type":"null"}]}},"$ref":"#/$defs/Result/anyOf/0"}`, "string"},
		{"escaped reference", `{"definitions":{"a/b~c":{"type":"boolean"}},"$ref":"#/definitions/a~1b~0c"}`, "boolean"},
		{"reference siblings", `{"$defs":{"Result":{"type":"string"}},"$ref":"#/$defs/Result","enum":["yes","no"]}`, `string & ("yes" | "no")`},
		{"const null", `{"const":null}`, "null"},
		{"object literal", `{"const":{"a-b":[1,"x",null]}}`, `{ "a-b": [1, "x", null]; }`},
		{"quoted keys and required", `{"type":"object","properties":{"a-b":{"type":"string"},"optional":{"type":"boolean","description":"Two\nlines"}},"required":["a-b"],"additionalProperties":false}`, "{\n  \"a-b\": string;\n  optional?: boolean; // Two lines\n}"},
		{"fixed and map", `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":{"type":"string"}}`, "{\n  count: number;\n  [key: string]: string | number;\n}"},
		{"file alias", `{"type":"string","format":"agent-file"}`, "FilePath"},
		{"directory alias", `{"type":"string","format":"agent-dir"}`, "DirPath"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Type(json.RawMessage(tc.schema))
			if err != nil || got != tc.want {
				t.Fatalf("Type() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestTypeRejectsInvalidSchemas(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"invalid JSON", `{"type":`, "invalid JSON"},
		{"null", `null`, "object or boolean"},
		{"scalar", `42`, "invalid schema"},
		{"array", `[]`, "invalid schema"},
		{"type scalar", `{"type":3}`, "type must"},
		{"type null", `{"type":null}`, "type must not"},
		{"type empty", `{"type":[]}`, "type must"},
		{"type element", `{"type":["string",3]}`, "type must"},
		{"type name", `{"type":"str"}`, "invalid schema type"},
		{"map value", `{"type":"object","additionalProperties":42}`, "additionalProperties"},
		{"map null", `{"additionalProperties":null}`, "additionalProperties"},
		{"property", `{"properties":{"bad":42}}`, `property "bad"`},
		{"properties null", `{"properties":null}`, "properties must not"},
		{"required", `{"type":"object","required":[1]}`, "invalid schema"},
		{"items", `{"type":"array","items":null}`, "items"},
		{"union empty", `{"anyOf":[]}`, "anyOf must be nonempty"},
		{"union null", `{"oneOf":null}`, "oneOf must not"},
		{"union element", `{"anyOf":[{"type":"string"},null]}`, "anyOf[1]"},
		{"enum empty", `{"enum":[]}`, "enum must be nonempty"},
		{"missing reference", `{"$ref":"#/$defs/Missing"}`, "unresolved $ref"},
		{"remote reference", `{"$ref":"https://example.com/schema"}`, "unsupported $ref"},
		{"recursive reference", `{"$defs":{"Node":{"properties":{"next":{"$ref":"#/$defs/Node"}}}},"$ref":"#/$defs/Node"}`, "recursive $ref"},
		{"tuple", `{"type":"array","prefixItems":[{"type":"string"}]}`, "prefixItems"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Type(json.RawMessage(tc.schema))
			if err == nil || got != "" || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Type() = %q, %v; want error containing %q and no fallback", got, err, tc.want)
			}
		})
	}
}
