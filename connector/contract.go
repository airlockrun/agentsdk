package connector

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

const (
	PlatformLinuxAMD64   = protocol.PlatformLinuxAMD64
	PlatformLinuxARM64   = protocol.PlatformLinuxARM64
	PlatformLinuxARMv7   = protocol.PlatformLinuxARMv7
	PlatformDarwinAMD64  = protocol.PlatformDarwinAMD64
	PlatformDarwinARM64  = protocol.PlatformDarwinARM64
	PlatformWindowsAMD64 = protocol.PlatformWindowsAMD64
	PlatformWindowsARM64 = protocol.PlatformWindowsARM64
)

type Contract struct {
	id string
}

func DefineContract(id string) Contract {
	if err := protocol.ValidateContractID(id); err != nil {
		panic(err)
	}
	return Contract{id: id}
}

func (c Contract) ID() string { return c.id }

type CommandOptions struct {
	Revision       int
	Description    string
	Mode           protocol.CommandMode
	Timeout        time.Duration
	MaxInputBytes  int64
	MaxOutputBytes int64
	Idempotent     bool
}

type Command[In, Out any] struct {
	contract       Contract
	descriptor     protocol.CommandDescriptor
	timeout        time.Duration
	maxInputBytes  int64
	maxOutputBytes int64
	idempotent     bool
}

func DefineCommand[In, Out any](contract Contract, name string, options CommandOptions) Command[In, Out] {
	if contract.id == "" {
		panic("connector: command contract is required")
	}
	if err := protocol.ValidateName("command", name); err != nil {
		panic(err)
	}
	if options.Revision < 1 {
		panic("connector: command revision must be at least 1")
	}
	if options.Mode == "" {
		options.Mode = protocol.CommandModeUnary
	}
	if options.Mode != protocol.CommandModeUnary && options.Mode != protocol.CommandModeJob {
		panic("connector: command mode must be unary or job")
	}
	if options.Timeout < 0 || options.MaxInputBytes < 0 || options.MaxOutputBytes < 0 {
		panic("connector: command limits cannot be negative")
	}
	if options.MaxInputBytes > protocol.MaxJobPayloadBytes || options.MaxOutputBytes > protocol.MaxJobPayloadBytes {
		panic(fmt.Sprintf("connector: command payload limits cannot exceed %d bytes", protocol.MaxJobPayloadBytes))
	}
	inSchema := schemaFor(reflect.TypeOf((*In)(nil)).Elem())
	outSchema := schemaFor(reflect.TypeOf((*Out)(nil)).Elem())
	return Command[In, Out]{
		contract: contract,
		descriptor: protocol.CommandDescriptor{
			Name: name, Revision: options.Revision, Description: options.Description, Mode: options.Mode,
			InputSchema: inSchema, OutputSchema: outSchema,
			InputSchemaHash: hash(inSchema), OutputSchemaHash: hash(outSchema),
		},
		timeout: options.Timeout, maxInputBytes: options.MaxInputBytes,
		maxOutputBytes: options.MaxOutputBytes, idempotent: options.Idempotent,
	}
}

func (c Command[In, Out]) Name() string               { return c.descriptor.Name }
func (c Command[In, Out]) ContractID() string         { return c.contract.id }
func (c Command[In, Out]) Revision() int              { return c.descriptor.Revision }
func (c Command[In, Out]) Mode() protocol.CommandMode { return c.descriptor.Mode }
func (c Command[In, Out]) Descriptor() protocol.CommandDescriptor {
	return cloneCommandDescriptor(c.descriptor)
}
func (c Command[In, Out]) requirement() requirement {
	return requirement{contractID: c.contract.id, command: &c.descriptor}
}

type Handler[In, Out any] func(context Context, input In) (Out, error)

// Context is the command execution context. It exposes progress only for job
// commands and otherwise behaves as a standard context.Context.
type Context interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
	Progress(phase, message string, completed, total int64) error
	JobID() string
	IdempotencyID() string
}

func (c Command[In, Out]) Handle(runtime *Runtime, handler Handler[In, Out]) {
	if runtime == nil || handler == nil {
		panic("connector: command runtime and handler are required")
	}
	runtime.registerCommand(commandRegistration{
		descriptor: c.Descriptor(), timeout: c.timeout, maxInputBytes: c.maxInputBytes,
		maxOutputBytes: c.maxOutputBytes, idempotent: c.idempotent,
		handle: func(ctx Context, raw json.RawMessage) (json.RawMessage, error) {
			var input In
			if err := strictUnmarshal(raw, &input); err != nil {
				return nil, fmt.Errorf("decode command %s input: %w", c.Name(), err)
			}
			output, err := handler(ctx, input)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(output)
			if err != nil {
				return nil, fmt.Errorf("encode command %s output: %w", c.Name(), err)
			}
			return encoded, nil
		},
	})
}

type DirectoryOptions struct {
	Revision    int
	Description string
	Read        bool
	Write       bool
	List        bool
}

type Directory struct {
	contract   Contract
	descriptor protocol.DirectoryDescriptor
}

func DefineDirectory(contract Contract, name string, options DirectoryOptions) Directory {
	if contract.id == "" {
		panic("connector: directory contract is required")
	}
	descriptor := protocol.DirectoryDescriptor{
		Name: name, Revision: options.Revision, Description: options.Description,
		Read: options.Read, Write: options.Write, List: options.List,
	}
	if err := protocol.ValidateRequirement(protocol.Requirement{ContractID: contract.id, Directories: []protocol.DirectoryDescriptor{descriptor}}); err != nil {
		panic(err)
	}
	return Directory{contract: contract, descriptor: descriptor}
}

func (d Directory) Name() string                             { return d.descriptor.Name }
func (d Directory) ContractID() string                       { return d.contract.id }
func (d Directory) Revision() int                            { return d.descriptor.Revision }
func (d Directory) Descriptor() protocol.DirectoryDescriptor { return d.descriptor }
func (d Directory) requirement() requirement {
	return requirement{contractID: d.contract.id, directory: &d.descriptor}
}

func (d Directory) Handle(runtime *Runtime, provider *LocalDirectoryProvider) {
	if runtime == nil || provider == nil {
		panic("connector: directory runtime and provider are required")
	}
	runtime.registerDirectory(directoryRegistration{descriptor: d.descriptor, provider: provider})
}

type Requirement interface {
	requirement() requirement
}

type requirement struct {
	contractID string
	command    *protocol.CommandDescriptor
	directory  *protocol.DirectoryDescriptor
}

func Require(items ...Requirement) protocol.Requirement {
	if len(items) == 0 {
		panic("connector: Require needs at least one command or directory")
	}
	var result protocol.Requirement
	for _, item := range items {
		if item == nil {
			panic("connector: nil requirement")
		}
		value := item.requirement()
		if result.ContractID == "" {
			result.ContractID = value.contractID
		} else if result.ContractID != value.contractID {
			panic("connector: one requirement cannot combine contract IDs")
		}
		if value.command != nil {
			result.Commands = append(result.Commands, cloneCommandDescriptor(*value.command))
		}
		if value.directory != nil {
			result.Directories = append(result.Directories, *value.directory)
		}
	}
	sort.Slice(result.Commands, func(i, j int) bool { return result.Commands[i].Name < result.Commands[j].Name })
	sort.Slice(result.Directories, func(i, j int) bool { return result.Directories[i].Name < result.Directories[j].Name })
	if err := protocol.ValidateRequirement(result); err != nil {
		panic(err)
	}
	return result
}

func schemaFor(value reflect.Type) json.RawMessage {
	schema := reflectSchema(value, make(map[reflect.Type]bool))
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("connector: encode schema: " + err.Error())
	}
	return encoded
}

func reflectSchema(value reflect.Type, visiting map[reflect.Type]bool) any {
	validateJSONType(value)
	switch value.Kind() {
	case reflect.Int, reflect.Uint, reflect.Uintptr:
		panic(fmt.Sprintf("connector: architecture-sized integer type %s is unsupported in command contracts; use a fixed-width integer type", value))
	}
	if visiting[value] {
		panic("connector: recursive command types are unsupported: " + value.String())
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Struct, reflect.Array, reflect.Slice, reflect.Map:
		visiting[value] = true
		defer delete(visiting, value)
	}
	if value.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{reflectSchema(value.Elem(), visiting), map[string]any{"type": "null"}}}
	}
	switch value.Kind() {
	case reflect.Struct:
		properties := make(map[string]any)
		seenNames := make(map[string]string)
		var required []string
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if field.Anonymous {
				panic("connector: anonymous fields are unsupported because encoding/json conflict resolution is not a stable schema contract: " + value.String() + "." + field.Name)
			}
			if field.PkgPath != "" {
				continue
			}
			name, options := parseJSONTag(field)
			if name == "-" {
				continue
			}
			if previous := seenNames[name]; previous != "" {
				panic(fmt.Sprintf("connector: fields %s and %s encode to duplicate JSON name %q", previous, field.Name, name))
			}
			seenNames[name] = field.Name
			properties[name] = reflectSchema(field.Type, visiting)
			if !options["omitempty"] && !options["omitzero"] {
				required = append(required, name)
			}
		}
		sort.Strings(required)
		result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			result["required"] = required
		}
		return result
	case reflect.Array, reflect.Slice:
		if value.Kind() == reflect.Slice && value.Elem().Kind() == reflect.Uint8 {
			panic("connector: []byte and json.RawMessage are unsupported in command contracts; use a string or a connector directory transfer")
		}
		array := map[string]any{"type": "array", "items": reflectSchema(value.Elem(), visiting)}
		if value.Kind() == reflect.Slice {
			return map[string]any{"anyOf": []any{array, map[string]any{"type": "null"}}}
		}
		return array
	case reflect.Map:
		if value.Key().Kind() != reflect.String {
			panic("connector: command maps must have string keys")
		}
		object := map[string]any{"type": "object", "additionalProperties": reflectSchema(value.Elem(), visiting)}
		return map[string]any{"anyOf": []any{object, map[string]any{"type": "null"}}}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	}
	panic("connector: unsupported command type " + value.String())
}

func parseJSONTag(field reflect.StructField) (string, map[string]bool) {
	parts := strings.Split(field.Tag.Get("json"), ",")
	name := parts[0]
	if name == "" {
		name = field.Name
	}
	options := make(map[string]bool)
	for _, option := range parts[1:] {
		if option != "omitempty" && option != "omitzero" && option != "" {
			panic(fmt.Sprintf("connector: field %s has unsupported json tag option %q", field.Name, option))
		}
		options[option] = true
	}
	if name == "" || strings.ContainsAny(name, "\\\",\x00\r\n") {
		panic(fmt.Sprintf("connector: field %s has an unsupported json name %q", field.Name, name))
	}
	return name, options
}

func validateJSONType(value reflect.Type) {
	if value == reflect.TypeOf(json.Number("")) {
		panic("connector: json.Number is unsupported in command contracts")
	}
	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	implements := func(candidate reflect.Type) bool {
		return candidate.Implements(jsonMarshaler) || candidate.Implements(jsonUnmarshaler) || candidate.Implements(textMarshaler) || candidate.Implements(textUnmarshaler)
	}
	if implements(value) || (value.Kind() != reflect.Pointer && implements(reflect.PointerTo(value))) {
		panic("connector: custom JSON or text marshalers are unsupported in command contracts: " + value.String())
	}
}

func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func cloneCommandDescriptor(value protocol.CommandDescriptor) protocol.CommandDescriptor {
	value.InputSchema = append(json.RawMessage(nil), value.InputSchema...)
	value.OutputSchema = append(json.RawMessage(nil), value.OutputSchema...)
	return value
}
