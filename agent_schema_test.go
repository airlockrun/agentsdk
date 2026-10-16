package agentsdk

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
)

type referencedAgentArray [2]int8

func TestAgentFloat64BoundsExactScientificNotation(t *testing.T) {
	lo, hi := agentNumericBounds(reflect.TypeFor[float64]())
	integer, _ := new(big.Float).SetFloat64(math.MaxFloat64).Int(nil)
	for _, tc := range []struct {
		name     string
		bound    json.Number
		negative bool
	}{{"maximum", hi, false}, {"minimum", lo, true}} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(string(tc.bound), "e308") {
				t.Fatal("float bound is not scientific notation")
			}
			rational, ok := new(big.Rat).SetString(string(tc.bound))
			if !ok {
				t.Fatal("invalid numeric bound")
			}
			want := new(big.Rat).SetInt(integer)
			if tc.negative {
				want.Neg(want)
			}
			if rational.Cmp(want) != 0 {
				t.Fatal("float bound lost precision")
			}
			var value float64
			if err := json.Unmarshal([]byte(tc.bound), &value); err != nil {
				t.Fatal(err)
			}
			max := math.MaxFloat64
			if tc.negative {
				max = -max
			}
			if value != max {
				t.Fatal("bound is not maximum finite Float64")
			}
		})
	}
}

func init() {
	schema.RegisterTypeOverride(reflect.TypeFor[referencedAgentArray](), func(s *schema.Schema) {
		s.Items = &schema.Schema{Ref: "#/$defs/element"}
		s.Definitions = map[string]*schema.Schema{"element": schema.Integer()}
	})
}

func TestAgentTypeSchemaPreservesReferenceRoot(t *testing.T) {
	raw := agentTypeSchema(reflect.TypeFor[referencedAgentArray]())
	var s struct {
		Items struct {
			Ref string `json:"$ref"`
		} `json:"items"`
		Defs  map[string]json.RawMessage `json:"$defs"`
		AllOf []struct {
			MinItems int                    `json:"minItems"`
			MaxItems int                    `json:"maxItems"`
			Items    map[string]json.Number `json:"items"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Items.Ref != "#/$defs/element" || string(s.Defs["element"]) != `{"type":"integer"}` {
		t.Fatalf("reference root changed: %s", raw)
	}
	if len(s.AllOf) != 1 || s.AllOf[0].MinItems != 2 || s.AllOf[0].MaxItems != 2 || len(s.AllOf[0].Items) != 0 {
		t.Fatalf("missing exact constraints: %s", raw)
	}
}

func TestAgentTypeSchemaNoInferredNumericBounds(t *testing.T) {
	for _, tc := range []struct {
		value any
		kind  string
	}{
		{int8(0), "integer"}, {int16(0), "integer"}, {int32(0), "integer"}, {int64(0), "integer"}, {int(0), "integer"},
		{uint8(0), "integer"}, {uint16(0), "integer"}, {uint32(0), "integer"}, {uint64(0), "integer"}, {uint(0), "integer"},
		{float32(0), "number"}, {float64(0), "number"},
	} {
		t.Run(reflect.TypeOf(tc.value).String(), func(t *testing.T) {
			raw := agentTypeSchema(reflect.TypeOf(tc.value))
			var s map[string]any
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			if len(s) != 1 || s["type"] != tc.kind {
				t.Fatalf("schema=%s", raw)
			}
		})
	}
}

func TestAgentTypeSchemaPreservesDeclaredNumericBounds(t *testing.T) {
	type bounded struct {
		Count    int  `json:"count" minimum:"0" maximum:"1000000"`
		Optional *int `json:"optional" minimum:"0" maximum:"1000000"`
		Byte     int8 `json:"byte" minimum:"-1000" maximum:"1000"`
	}
	raw := agentTypeSchema(reflect.TypeFor[bounded]())
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["allOf"]; ok {
		t.Fatalf("inferred bounds remain: %s", raw)
	}
	properties := root["properties"].(map[string]any)
	count := properties["count"].(map[string]any)
	optional := properties["optional"].(map[string]any)
	for _, v := range []map[string]any{count, optional} {
		if v["minimum"] != json.Number("0") || v["maximum"] != json.Number("1000000") {
			t.Fatalf("wide bound remains: %s", raw)
		}
	}
	byteBounds := properties["byte"].(map[string]any)
	if byteBounds["minimum"] != json.Number("-1000") || byteBounds["maximum"] != json.Number("1000") {
		t.Fatalf("declared annotations changed: %s", raw)
	}
	if err := validateAgentTypeValue(reflect.TypeFor[bounded](), json.RawMessage(`{"byte":128}`)); err == nil {
		t.Fatal("native overflow accepted without inferred schema bounds")
	}
}

func TestAgentTypeSchemaNumericWidthsAreNativeValidationOnly(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[int8](), reflect.TypeFor[int64]()},
		{reflect.TypeFor[uint8](), reflect.TypeFor[uint64]()},
		{reflect.TypeFor[float32](), reflect.TypeFor[float64]()},
	} {
		if !bytes.Equal(agentTypeSchema(pair[0]), agentTypeSchema(pair[1])) {
			t.Fatalf("numeric width leaked into schema for %v", pair)
		}
	}
	if err := validateAgentTypeValue(reflect.TypeFor[int8](), json.RawMessage(`128`)); err == nil {
		t.Fatal("int8 overflow accepted")
	}
	if err := validateAgentTypeValue(reflect.TypeFor[int64](), json.RawMessage(`128`)); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTypeSchemaStringAndSliceDefaults(t *testing.T) {
	type plain struct {
		Text   string   `json:"text"`
		Values []string `json:"values"`
		Count  *int     `json:"count,omitempty"`
	}
	raw := agentTypeSchema(reflect.TypeFor[plain]())
	for _, keyword := range []string{`"minimum"`, `"maximum"`, `"minLength"`, `"maxLength"`, `"pattern"`, `"minItems"`, `"maxItems"`, `"uniqueItems"`, `"allOf"`} {
		if bytes.Contains(raw, []byte(keyword)) {
			t.Fatalf("inferred validation %s: %s", keyword, raw)
		}
	}
}

func TestAgentTypeSchemaNestedConstraints(t *testing.T) {
	type payload struct {
		Values  *[1][2]int8 `json:"values,omitempty" description:"Exact values"`
		Slice   []uint64    `json:"slice"`
		Zero    [0]string   `json:"zero"`
		Ignored [5]int16    `json:"-"`
	}
	raw := agentTypeSchema(reflect.TypeFor[payload]())
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		t.Fatal(err)
	}
	c := root["allOf"].([]any)[0].(map[string]any)["properties"].(map[string]any)
	if len(c) != 2 {
		t.Fatalf("constraints=%v", c)
	}
	array := c["values"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)
	inner := array["items"].(map[string]any)
	if array["minItems"] != json.Number("1") || array["maxItems"] != json.Number("1") || inner["minItems"] != json.Number("2") || inner["maxItems"] != json.Number("2") {
		t.Fatalf("schema=%s", raw)
	}
	if _, ok := c["slice"]; ok {
		t.Fatal("slice has inferred numeric bounds")
	}
	zero := c["zero"].(map[string]any)
	if zero["minItems"] != json.Number("0") || zero["maxItems"] != json.Number("0") {
		t.Fatal("zero array constraint omitted")
	}
	if root["properties"].(map[string]any)["values"].(map[string]any)["description"] != "Exact values" {
		t.Fatal("generated metadata lost")
	}
}

func TestValidateAgentTypeValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typ   reflect.Type
		raw   string
		valid bool
	}{
		{"int64 max", reflect.TypeFor[int64](), `9223372036854775807`, true},
		{"int64 overflow", reflect.TypeFor[int64](), `9223372036854775808`, false},
		{"int64 min", reflect.TypeFor[int64](), `-9223372036854775808`, true},
		{"int64 underflow", reflect.TypeFor[int64](), `-9223372036854775809`, false},
		{"uint64 max", reflect.TypeFor[uint64](), `18446744073709551615`, true},
		{"uint64 overflow", reflect.TypeFor[uint64](), `18446744073709551616`, false},
		{"uint negative", reflect.TypeFor[uint64](), `-1`, false},
		{"fraction", reflect.TypeFor[int8](), `1.5`, false},
		{"float32 max", reflect.TypeFor[float32](), `3.4028235e38`, true},
		{"float32 overflow", reflect.TypeFor[float32](), `3.5e38`, false},
		{"float64 max", reflect.TypeFor[float64](), `1.7976931348623157e308`, true},
		{"float64 overflow", reflect.TypeFor[float64](), `1.8e308`, false},
		{"zero array", reflect.TypeFor[[0]string](), `[]`, true},
		{"zero array excess", reflect.TypeFor[[0]string](), `["x"]`, false},
		{"pointer nil", reflect.TypeFor[*[1]int8](), `null`, true},
		{"array nil", reflect.TypeFor[[1]int8](), `null`, false},
		{"nested pointers", reflect.TypeFor[[]*[1]int8](), `[null,[127]]`, true},
		{"nested pointers excess", reflect.TypeFor[[]*[1]int8](), `[[127,0]]`, false},
		{"nested pointers overflow", reflect.TypeFor[[]*[1]int8](), `[[128]]`, false},
		{"invalid JSON", reflect.TypeFor[[1]string](), `[`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentTypeValue(tc.typ, json.RawMessage(tc.raw))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestAgentTypeSchemaContractHashes(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[[1]string](), reflect.TypeFor[[2]string]()},
		{reflect.TypeFor[*[1][2]int8](), reflect.TypeFor[*[1][3]int8]()},
	} {
		t.Run(pair[0].String()+" vs "+pair[1].String(), func(t *testing.T) {
			for _, input := range []bool{true, false} {
				d := wire.AgentDefinition{InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)}
				var hashes [2]string
				for i, typ := range pair {
					if input {
						d.InputSchema = agentTypeSchema(typ)
					} else {
						d.OutputSchema = agentTypeSchema(typ)
					}
					var err error
					hashes[i], err = wire.AgentDefinitionHash(d)
					if err != nil {
						t.Fatal(err)
					}
				}
				if hashes[0] == hashes[1] {
					t.Fatalf("input=%v: fixed array lengths share hash", input)
				}
			}
		})
	}
}

func TestAgentTypeSchemaHashPreservesIntegerPrecision(t *testing.T) {
	type explicit struct {
		Value uint64 `json:"value" maximum:"18446744073709551615"`
	}
	d := wire.AgentDefinition{InputSchema: agentTypeSchema(reflect.TypeFor[int64]()), OutputSchema: agentTypeSchema(reflect.TypeFor[explicit]())}
	hash, err := wire.AgentDefinitionHash(d)
	if err != nil {
		t.Fatal(err)
	}
	d.OutputSchema = bytes.ReplaceAll(d.OutputSchema, []byte("18446744073709551615"), []byte("18446744073709551614"))
	changed, err := wire.AgentDefinitionHash(d)
	if err != nil {
		t.Fatal(err)
	}
	if hash == changed {
		t.Fatal("wire hash rounded neighboring uint64 bounds")
	}
}
