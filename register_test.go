package agentsdk

import (
	"context"
	"testing"
)

type addIn struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type addOut struct {
	Sum float64 `json:"sum"`
}

func TestRegisterTool(t *testing.T) {
	t.Run("stores tool with access", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "add",
			Description: "Add two numbers.",
			Execute: func(ctx context.Context, in addIn) (addOut, error) {
				return addOut{Sum: in.X + in.Y}, nil
			},
			Access: AccessUser,
		})

		if len(a.tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(a.tools))
		}
		tt := a.tools["add"]
		if tt == nil {
			t.Fatal("expected registered tool for add")
		}
		if tt.Access != AccessUser {
			t.Fatalf("expected AccessUser, got %q", tt.Access)
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
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "noop",
			Description: "No op.",
			Execute: func(ctx context.Context, in addIn) (addOut, error) {
				return addOut{}, nil
			},
		})
		if a.tools["noop"].Access != AccessUser {
			t.Fatalf("default access = %q, want AccessUser", a.tools["noop"].Access)
		}
	})

	t.Run("panics on duplicate name", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "dup",
			Description: "dup",
			Execute:     func(ctx context.Context, in addIn) (addOut, error) { return addOut{}, nil },
		})
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate name")
			}
		}()
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "dup",
			Description: "dup again",
			Execute:     func(ctx context.Context, in addIn) (addOut, error) { return addOut{}, nil },
		})
	})

	t.Run("panics on missing Execute", func(t *testing.T) {
		a, _ := testAgent(t)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on missing Execute")
			}
		}()
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "bad",
			Description: "no exec",
		})
	})

	t.Run("panics on empty name", func(t *testing.T) {
		a, _ := testAgent(t)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on empty name")
			}
		}()
		a.RegisterTool(&Tool[addIn, addOut]{
			Description: "no name",
			Execute:     func(ctx context.Context, in addIn) (addOut, error) { return addOut{}, nil },
		})
	})

	t.Run("Execute round-trips input/output via JSON", func(t *testing.T) {
		a, _ := testAgent(t)
		a.RegisterTool(&Tool[addIn, addOut]{
			Name:        "add_rt",
			Description: "Add two numbers.",
			Execute: func(ctx context.Context, in addIn) (addOut, error) {
				return addOut{Sum: in.X + in.Y}, nil
			},
		})
		tt := a.tools["add_rt"]
		out, err := tt.Execute(context.Background(), []byte(`{"x":3,"y":4}`))
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if out != `{"sum":7}` {
			t.Fatalf("got %q, want {\"sum\":7}", out)
		}
	})
}
