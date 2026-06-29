package agentsdk

import (
	"context"
	"testing"

	"github.com/airlockrun/goai/tool"
)

type addIn struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type addOut struct {
	Sum float64 `json:"sum"`
}

func addTool(name, desc string) tool.Tool {
	return tool.Typed[addIn, addOut](name).
		Description(desc).
		Execute(func(ctx context.Context, in addIn) (addOut, error) {
			return addOut{Sum: in.X + in.Y}, nil
		}).
		Build()
}

func TestRegisterTool(t *testing.T) {
	t.Run("stores tool with access", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(addTool("add", "Add two numbers."), AccessUser)

		if len(a.tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(a.tools))
		}
		tt := a.tools["add"]
		if tt == nil {
			t.Fatal("expected registered tool for add")
		}
		if tt.access != AccessUser {
			t.Fatalf("expected AccessUser, got %q", tt.access)
		}
		if tt.Execute == nil {
			t.Fatal("expected Execute to be set")
		}
		if tt.InputSchema == nil || tt.OutputSchema == nil {
			t.Fatal("expected input/output schemas to be generated")
		}
	})

	t.Run("access defaults to AccessUser", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(addTool("noop", "No op."), "")
		if a.tools["noop"].access != AccessUser {
			t.Fatalf("default access = %q, want AccessUser", a.tools["noop"].access)
		}
	})

	t.Run("panics on duplicate name", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(addTool("dup", "dup"), AccessUser)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate name")
			}
		}()
		a.RegisterTool(addTool("dup", "dup again"), AccessUser)
	})

	t.Run("panics on missing Execute", func(t *testing.T) {
		a, _ := testAgent(t)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on missing Execute")
			}
		}()
		a.RegisterTool(tool.Tool{Name: "bad", Description: "no exec"}, AccessUser)
	})

	t.Run("panics on empty name", func(t *testing.T) {
		a, _ := testAgent(t)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on empty name")
			}
		}()
		a.RegisterTool(addTool("", "no name"), AccessUser)
	})

	t.Run("Execute round-trips input/output via JSON", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(addTool("add_rt", "Add two numbers."), AccessUser)
		tt := a.tools["add_rt"]
		out, err := tt.Execute(context.Background(), []byte(`{"x":3,"y":4}`), tool.CallOptions{})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if out.Output != `{"sum":7}` {
			t.Fatalf("got %q, want {\"sum\":7}", out.Output)
		}
	})
}
