package connector

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

type customJSON string

func (customJSON) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

type customText string

var _ encoding.TextMarshaler = customText("")

func (customText) MarshalText() ([]byte, error) { return nil, nil }

type embeddedFields struct{ contractInput }
type byteFields struct {
	Data []byte `json:"data"`
}
type rawFields struct {
	Data json.RawMessage `json:"data"`
}
type stringOption struct {
	Count int64 `json:"count,string"`
}
type interfaceField struct {
	Value any `json:"value"`
}
type numberField struct {
	Value json.Number `json:"value"`
}
type customFields struct {
	Value customJSON `json:"value"`
}
type customTextFields struct {
	Value customText `json:"value"`
}

type contractInput struct {
	Name  string `json:"name"`
	Limit int64  `json:"limit,omitempty"`
}

func TestCommandSchemaRejectsEncodingAmbiguity(t *testing.T) {
	type recursiveMap map[string]recursiveMap
	type recursiveSlice []recursiveSlice
	tests := []struct {
		name   string
		define func()
	}{
		{name: "custom JSON", define: func() {
			DefineCommand[customFields, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "custom text", define: func() {
			DefineCommand[customTextFields, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "embedded", define: func() {
			DefineCommand[embeddedFields, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "duplicate", define: func() {
			typeOf := reflect.StructOf([]reflect.StructField{{Name: "A", Type: reflect.TypeOf(""), Tag: `json:"same"`}, {Name: "B", Type: reflect.TypeOf(""), Tag: `json:"same"`}})
			schemaFor(typeOf)
		}},
		{name: "bytes", define: func() {
			DefineCommand[byteFields, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "raw JSON", define: func() {
			DefineCommand[rawFields, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "string option", define: func() {
			DefineCommand[stringOption, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "interface", define: func() {
			DefineCommand[interfaceField, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "json number", define: func() {
			DefineCommand[numberField, contractOutput](DefineContract("io.airlockrun.schema_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "recursive map", define: func() { schemaFor(reflect.TypeOf(recursiveMap{})) }},
		{name: "recursive slice", define: func() { schemaFor(reflect.TypeOf(recursiveSlice{})) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("definition did not panic")
				}
			}()
			test.define()
		})
	}
}

func TestCommandSchemaMatchesJSONNamesAndOptionalFields(t *testing.T) {
	type input struct {
		Required string `json:"required"`
		Optional int64  `json:"optional,omitempty"`
		Zero     bool   `json:"zero,omitzero"`
	}
	command := DefineCommand[input, contractOutput](DefineContract("io.airlockrun.schema_match"), "run", CommandOptions{Revision: 1})
	if !bytes.Contains(command.Descriptor().InputSchema, []byte(`"required":["required"]`)) {
		t.Fatalf("schema = %s", command.Descriptor().InputSchema)
	}
}

func TestCommandSchemaRejectsArchitectureSizedIntegers(t *testing.T) {
	type namedInt int
	type nested struct {
		Value int `json:"value"`
	}
	tests := []struct {
		name   string
		define func()
	}{
		{name: "int input", define: func() { schemaFor(reflect.TypeOf(int(0))) }},
		{name: "uint output", define: func() {
			DefineCommand[contractInput, uint](DefineContract("io.airlockrun.integer_test"), "run", CommandOptions{Revision: 1})
		}},
		{name: "uintptr", define: func() { schemaFor(reflect.TypeOf(uintptr(0))) }},
		{name: "named int", define: func() { schemaFor(reflect.TypeOf(namedInt(0))) }},
		{name: "nested", define: func() { schemaFor(reflect.TypeOf(nested{})) }},
		{name: "pointer", define: func() { schemaFor(reflect.TypeOf((*int)(nil))) }},
		{name: "slice", define: func() { schemaFor(reflect.TypeOf([]int{})) }},
		{name: "map value", define: func() { schemaFor(reflect.TypeOf(map[string]int{})) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				message := fmt.Sprint(recover())
				if !strings.Contains(message, "architecture-sized integer") || !strings.Contains(message, "fixed-width integer") {
					t.Fatalf("panic = %q", message)
				}
			}()
			test.define()
		})
	}
}

func TestCommandSchemaAcceptsFixedWidthIntegers(t *testing.T) {
	type fixedWidth struct {
		Signed   int32             `json:"signed"`
		Unsigned uint64            `json:"unsigned"`
		Values   []int64           `json:"values"`
		ByName   map[string]uint32 `json:"byName"`
		Ignored  int               `json:"-"`
	}
	schemaFor(reflect.TypeOf(fixedWidth{}))
}

type contractOutput struct {
	OK bool `json:"ok"`
}

func TestCommandAndRequirement(t *testing.T) {
	contract := DefineContract("io.airlockrun.test_connector")
	command := DefineCommand[contractInput, contractOutput](contract, "run", CommandOptions{Revision: 2, Mode: protocol.CommandModeJob})
	directory := DefineDirectory(contract, "files", DirectoryOptions{Revision: 1, Read: true, List: true})
	requirement := Require(directory, command)
	if requirement.ContractID != contract.ID() || len(requirement.Commands) != 1 || len(requirement.Directories) != 1 {
		t.Fatalf("Require() = %+v", requirement)
	}
	if requirement.Commands[0].Name != "run" || len(requirement.Commands[0].InputSchemaHash) != 64 {
		t.Fatalf("command descriptor = %+v", requirement.Commands[0])
	}
	var schema map[string]any
	if err := json.Unmarshal(requirement.Commands[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	required := schema["required"].([]any)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("required = %#v", required)
	}
}

func TestDefineContractRejectsInvalid(t *testing.T) {
	tests := []string{"", "thing", "Com.Example.Thing"}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("DefineContract did not panic")
				}
			}()
			DefineContract(value)
		})
	}
}
