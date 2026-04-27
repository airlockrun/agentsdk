package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/airlockrun/goai/schema"
)

// AnyTool is the sealed interface produced by *Tool[In, Out]. The unexported
// toRegistered method means only generic Tool values can satisfy it —
// authors cannot roll their own AnyTool implementations. Used as the
// argument type for RegisterTool so a heterogeneous set of generic Tool
// values can share one entry point.
type AnyTool interface {
	toRegistered() *registeredTool
}

// Tool is the declarative description of a typed, schema-bearing capability
// the LLM can invoke via run_js. In and Out are Go structs whose JSON
// schemas are generated at registration time and rendered as TypeScript
// signatures in the system prompt. Execute receives the unmarshaled In and
// returns Out — the wrapper handles JSON boundaries on both sides.
//
// Construct with a pointer literal:
//
//	agent.RegisterTool(&agentsdk.Tool[SearchIn, SearchOut]{
//	    Name:        "search",
//	    Description: "Search the web.",
//	    Execute:     doSearch,
//	    Access:      agentsdk.AccessUser,
//	})
type Tool[In any, Out any] struct {
	Name          string
	Description   string
	Execute       func(ctx context.Context, input In) (Out, error)
	Access        Access
	InputExamples []In
}

// registeredTool is the internal, non-generic representation stored on *Agent.
// The wrapped Execute erases the generic parameters so the agent can hold a
// heterogeneous map of tools.
type registeredTool struct {
	Name          string
	Description   string
	Access        Access
	InputSchema   *schema.Schema
	OutputSchema  *schema.Schema
	InputExamples []json.RawMessage
	// Execute takes raw JSON input, unmarshals into the author's In struct,
	// calls the user function, marshals Out to a JSON string, and returns it.
	Execute func(ctx context.Context, raw json.RawMessage) (jsonOut string, err error)
}

// toRegistered finalizes the Tool declaration: validates required fields,
// generates input/output schemas via schema.MustFromType, and builds the
// type-erased Execute wrapper. Called once at RegisterTool time.
func (t *Tool[In, Out]) toRegistered() *registeredTool {
	if t == nil {
		panic("agentsdk: RegisterTool: nil *Tool")
	}
	if t.Name == "" {
		panic("agentsdk: RegisterTool: Name is required")
	}
	if t.Description == "" {
		panic(fmt.Sprintf("agentsdk: RegisterTool(%q): Description is required", t.Name))
	}
	if t.Execute == nil {
		panic(fmt.Sprintf("agentsdk: RegisterTool(%q): Execute is required", t.Name))
	}
	access := t.Access
	if access == "" {
		access = AccessUser
	}

	inSchema := mustSchemaFromType[In](t.Name, "input")
	outSchema := mustSchemaFromType[Out](t.Name, "output")

	examples := make([]json.RawMessage, 0, len(t.InputExamples))
	for i, ex := range t.InputExamples {
		raw, err := json.Marshal(ex)
		if err != nil {
			panic(fmt.Sprintf("agentsdk: RegisterTool(%q): InputExamples[%d]: %v", t.Name, i, err))
		}
		examples = append(examples, raw)
	}

	userFn := t.Execute
	name := t.Name
	exec := func(ctx context.Context, raw json.RawMessage) (string, error) {
		var in In
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", fmt.Errorf("%s: decode input: %w", name, err)
			}
		}
		out, err := userFn(ctx, in)
		if err != nil {
			return "", err
		}
		buf, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("%s: encode output: %w", name, err)
		}
		return string(buf), nil
	}

	return &registeredTool{
		Name:          name,
		Description:   t.Description,
		Access:        access,
		InputSchema:   inSchema,
		OutputSchema:  outSchema,
		InputExamples: examples,
		Execute:       exec,
	}
}

// mustSchemaFromType generates a schema from a zero value of T. Recursive
// types (pointer cycles in struct fields) make schema.MustFromType overflow
// the stack; catch that and surface a remediation hint.
func mustSchemaFromType[T any](toolName, kind string) *schema.Schema {
	var zero T
	defer func() {
		if r := recover(); r != nil {
			panic(fmt.Sprintf("agentsdk: RegisterTool(%q): %s schema generation failed: %v — check for recursive struct fields (use `json:\"-\"` to break cycles)", toolName, kind, r))
		}
	}()
	return schema.MustFromType(zero)
}
