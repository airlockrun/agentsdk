package agentsdk

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
)

type referencedAgentArray [2]int8

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
	if len(s.AllOf) != 1 || s.AllOf[0].MinItems != 2 || s.AllOf[0].MaxItems != 2 || s.AllOf[0].Items["minimum"] != "-128" || s.AllOf[0].Items["maximum"] != "127" {
		t.Fatalf("missing exact constraints: %s", raw)
	}
}

func TestAgentTypeSchemaNumericBounds(t *testing.T) {
	for _, tc := range []struct {
		value    any
		min, max string
	}{
		{int8(0), "-128", "127"}, {int16(0), "-32768", "32767"}, {int32(0), "-2147483648", "2147483647"},
		{int64(0), "-9223372036854775808", "9223372036854775807"},
		{uint8(0), "0", "255"}, {uint16(0), "0", "65535"}, {uint32(0), "0", "4294967295"},
		{uint64(0), "0", "18446744073709551615"},
		{int(0), strconv.FormatInt(math.MinInt, 10), strconv.FormatInt(math.MaxInt, 10)},
		{uint(0), "0", strconv.FormatUint(math.MaxUint, 10)},
		{float32(0), "-3.4028235e+38", "3.4028235e+38"},
	} {
		t.Run(reflect.TypeOf(tc.value).String(), func(t *testing.T) {
			raw := agentTypeSchema(reflect.TypeOf(tc.value))
			var s struct {
				AllOf []map[string]json.Number `json:"allOf"`
			}
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			if len(s.AllOf) != 1 || s.AllOf[0]["minimum"].String() != tc.min || s.AllOf[0]["maximum"].String() != tc.max {
				t.Fatalf("schema=%s", raw)
			}
		})
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
	if len(c) != 3 {
		t.Fatalf("constraints=%v", c)
	}
	array := c["values"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)
	inner := array["items"].(map[string]any)
	number := inner["items"].(map[string]any)
	if array["minItems"] != json.Number("1") || array["maxItems"] != json.Number("1") || inner["minItems"] != json.Number("2") || inner["maxItems"] != json.Number("2") || number["minimum"] != json.Number("-128") || number["maximum"] != json.Number("127") {
		t.Fatalf("schema=%s", raw)
	}
	if c["slice"].(map[string]any)["items"].(map[string]any)["maximum"] != json.Number("18446744073709551615") {
		t.Fatal("nested uint64 bound rounded")
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
		{reflect.TypeFor[int8](), reflect.TypeFor[int16]()},
		{reflect.TypeFor[uint32](), reflect.TypeFor[uint64]()},
		{reflect.TypeFor[float32](), reflect.TypeFor[float64]()},
		{reflect.TypeFor[*[1][2]int8](), reflect.TypeFor[*[1][2]int16]()},
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
					t.Fatalf("input=%v: widths/lengths share hash", input)
				}
			}
		})
	}
}

func TestAgentTypeSchemaHashPreservesIntegerPrecision(t *testing.T) {
	d := wire.AgentDefinition{InputSchema: agentTypeSchema(reflect.TypeFor[int64]()), OutputSchema: agentTypeSchema(reflect.TypeFor[uint64]())}
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
