package agentruntime

import (
	"encoding/json"
	"testing"
)

func TestValidateJSON(t *testing.T) {
	for _, tc := range []struct {
		name, schema, value string
		valid               bool
	}{
		{"object", `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`, `{"id":1}`, true},
		{"required", `{"type":"object","required":["id"]}`, `{}`, false},
		{"additional property", `{"type":"object","additionalProperties":false}`, `{"id":1}`, false},
		{"map schema", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"id":1}`, false},
		{"union", `{"type":["string","null"]}`, `null`, true},
		{"boolean true", `true`, `{"anything":true}`, true},
		{"boolean false", `false`, `null`, false},
		{"not", `{"not":{"type":"string"}}`, `42`, true},
		{"allOf", `{"allOf":[{"type":"integer"},{"minimum":3}]}`, `2`, false},
		{"anyOf", `{"anyOf":[{"type":"integer"},{"type":"boolean"}]}`, `true`, true},
		{"oneOf ambiguous", `{"oneOf":[{"type":"integer"},{"type":"number"}]}`, `1`, false},
		{"integer exponent", `{"type":"integer"}`, `1e3`, true},
		{"integer decimal", `{"type":"integer"}`, `1.5`, false},
		{"enum numeric equality", `{"enum":[{"v":1}]}`, `{"v":1.0}`, true},
		{"enum missing null property", `{"enum":[{"a":null}]}`, `{"b":null}`, false},
		{"array uniqueness", `{"type":"array","uniqueItems":true}`, `[1,1.0]`, false},
		{"array length", `{"type":"array","minItems":1,"maxItems":2,"items":{"type":"boolean"}}`, `[true,false]`, true},
		{"integer schema constraint", `{"type":"array","minItems":1.0}`, `[true]`, true},
		{"array item", `{"type":"array","items":{"type":"boolean"}}`, `[true,1]`, false},
		{"fixed array exact", `{"type":"array","minItems":2,"maxItems":2,"items":{"type":"integer","minimum":-128,"maximum":127}}`, `[-128,127]`, true},
		{"fixed array short", `{"type":"array","minItems":2,"maxItems":2}`, `[1]`, false},
		{"fixed array long", `{"type":"array","minItems":2,"maxItems":2}`, `[1,2,3]`, false},
		{"fixed zero array", `{"type":"array","minItems":0,"maxItems":0}`, `[]`, true},
		{"fixed zero array nonempty", `{"type":"array","minItems":0,"maxItems":0}`, `[1]`, false},
		{"array minimum string", `{"minItems":"2"}`, `[]`, false},
		{"array maximum string", `{"maxItems":"2"}`, `[]`, false},
		{"array minimum fraction", `{"minItems":1.5}`, `[]`, false},
		{"array maximum fraction", `{"maxItems":1.5}`, `[]`, false},
		{"array minimum negative", `{"minItems":-1}`, `[]`, false},
		{"array maximum negative", `{"maxItems":-1}`, `[]`, false},
		{"array minimum boolean", `{"minItems":true}`, `[]`, false},
		{"array maximum null", `{"maxItems":null}`, `[]`, false},
		{"string pattern", `{"type":"string","pattern":"^[a-z]+$","minLength":2,"maxLength":4}`, `"Ab"`, false},
		{"numeric constraints", `{"type":"number","exclusiveMinimum":1,"exclusiveMaximum":2,"multipleOf":0.1}`, `1.5`, true},
		{"int8 inclusive lower", `{"type":"integer","minimum":-128,"maximum":127}`, `-128`, true},
		{"int8 inclusive upper", `{"type":"integer","minimum":-128,"maximum":127}`, `127`, true},
		{"int8 below minimum", `{"type":"integer","minimum":-128,"maximum":127}`, `-129`, false},
		{"int8 above maximum", `{"type":"integer","minimum":-128,"maximum":127}`, `128`, false},
		{"integer string rejected", `{"type":"integer","minimum":0,"maximum":255}`, `"1"`, false},
		{"uint64 maximum", `{"type":"integer","minimum":0,"maximum":18446744073709551615}`, `18446744073709551615`, true},
		{"uint64 overflow", `{"type":"integer","minimum":0,"maximum":18446744073709551615}`, `18446744073709551616`, false},
		{"float inclusive lower", `{"type":"number","minimum":-0.5,"maximum":0.5}`, `-0.5`, true},
		{"float inclusive upper", `{"type":"number","minimum":-0.5,"maximum":0.5}`, `0.5`, true},
		{"float below minimum", `{"type":"number","minimum":-0.5,"maximum":0.5}`, `-0.5001`, false},
		{"float above maximum", `{"type":"number","minimum":-0.5,"maximum":0.5}`, `0.5001`, false},
		{"minimum string", `{"minimum":"0"}`, `1`, false},
		{"maximum string", `{"maximum":"2"}`, `1`, false},
		{"minimum boolean", `{"minimum":false}`, `1`, false},
		{"maximum null", `{"maximum":null}`, `1`, false},
		{"multipleOf", `{"multipleOf":0.1}`, `1.55`, false},
		{"local pointer escape", `{"$defs":{"a/b":{"type":"string"}},"$ref":"#/$defs/a~1b"}`, `"yes"`, true},
		{"unsupported assertion under not", `{"not":{"unevaluatedProperties":false}}`, `{}`, false},
		{"unsupported referenced assertion under not", `{"const":{"unevaluatedProperties":false},"not":{"$ref":"#/const"}}`, `{}`, false},
		{"unsupported assertion in optional property", `{"properties":{"optional":{"contains":true}}}`, `{}`, false},
		{"malformed optional property", `{"properties":{"optional":{"type":["string",42]}}}`, `{}`, false},
		{"malformed properties", `{"properties":42}`, `{}`, false},
		{"unresolved ref", `{"$ref":"#/$defs/missing"}`, `{}`, false},
		{"remote ref", `{"$ref":"https://example.test/schema"}`, `{}`, false},
		{"recursive schema without progress", `{"$ref":"#"}`, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateJSON(json.RawMessage(tc.schema), json.RawMessage(tc.value)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
