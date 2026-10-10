package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/airlockrun/goai/tool"
)

func TestRegisterRouteToolCorrespondence(t *testing.T) {
	for _, toolFirst := range []bool{true, false} {
		name := "route first"
		if toolFirst {
			name = "tool first"
		}
		t.Run(name, func(t *testing.T) {
			a, mock := testAgent(t)
			registered := addTool("save", "Save a record")
			if toolFirst {
				a.RegisterTool(registered, AccessAdmin)
			}
			route := &Route{Method: http.MethodPost, Path: "/save", Handler: func(http.ResponseWriter, *http.Request) error { return nil }, Access: AccessPublic, Description: "Save through HTTP", Tool: tool.Tool{Name: registered.Name}}
			a.RegisterRoute(route)
			route.Tool.Name = "mutated"
			if !toolFirst {
				a.RegisterTool(registered, AccessAdmin)
			}
			manifest := a.Manifest()
			if len(manifest.Routes) != 1 || manifest.Routes[0].ToolName != "save" || manifest.Routes[0].Access != "public" || manifest.Tools[0].Access != "admin" {
				t.Fatalf("correspondence or independent access lost: %+v", manifest)
			}
			if err := a.syncWithAirlock(t.Context()); err != nil {
				t.Fatal(err)
			}
			requests := mock.RequestsByPath("/api/agent/sync")
			var payload struct {
				Routes []map[string]any `json:"routes"`
			}
			if len(requests) != 1 {
				t.Fatalf("sync requests = %d", len(requests))
			}
			if err := json.Unmarshal(requests[0].Body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Routes[0]["tool_name"] != "save" {
				t.Fatalf("manifest wire correspondence = %+v", payload.Routes)
			}
		})
	}
}

func TestRouteToolValidationBeforeFreeze(t *testing.T) {
	for _, freeze := range []string{"handler", "manifest", "sync"} {
		t.Run(freeze, func(t *testing.T) {
			a, mock := testAgent(t)
			other, _ := testAgent(t)
			foreign := addTool("foreign", "Other app tool")
			other.RegisterTool(foreign, AccessUser)
			a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/", Handler: func(http.ResponseWriter, *http.Request) error { return nil }, Access: AccessUser, Description: "Home", Tool: foreign})
			expectPanicContains(t, `corresponding tool "foreign" must be registered on this app`, func() {
				switch freeze {
				case "handler":
					a.Handler()
				case "manifest":
					a.Manifest()
				case "sync":
					_ = a.syncWithAirlock(t.Context())
				}
			})
			if len(mock.Requests()) != 0 {
				t.Fatal("invalid correspondence reached the host")
			}
		})
	}
}

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

	t.Run("access is required", func(t *testing.T) {
		a, _ := testAgent(t)
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic for empty Access")
			}
		}()
		a.RegisterTool(addTool("noop", "No op."), "")
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

func TestRegisterToolFileInputs(t *testing.T) {
	a, _ := testAgent(t)
	uploads := a.RegisterDirectory("uploads", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser, Description: "Uploads",
	})
	a.RegisterTool(addTool("import_file", "Import a file."), AccessUser, WithFileInputs(uploads))
	if got := a.Manifest().Tools[0].FileInputDirectory; got != "uploads" {
		t.Fatalf("FileInputDirectory = %q, want uploads", got)
	}

	for _, tt := range []struct {
		name   string
		handle func(*Agent) *DirectoryHandle
	}{
		{name: "nil"},
		{name: "foreign", handle: func(*Agent) *DirectoryHandle {
			other, _ := testAgent(t)
			return other.RegisterDirectory("uploads", DirectoryOpts{
				Read: AccessUser, Write: AccessUser, List: AccessUser, Description: "Uploads",
			})
		}},
		{name: "unregistered", handle: func(a *Agent) *DirectoryHandle {
			return &DirectoryHandle{agent: a, path: "missing"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testAgent(t)
			var handle *DirectoryHandle
			if tt.handle != nil {
				handle = tt.handle(a)
			}
			expectPanicContains(t, "file input directory must be registered on this app", func() {
				a.RegisterTool(addTool("bad_"+tt.name, "Bad file input directory."), AccessUser, WithFileInputs(handle))
			})
		})
	}
}

func TestRegisterDirectoryReservedTmpReturnsHandle(t *testing.T) {
	a, _ := testAgent(t)
	h := a.RegisterDirectory("tmp", DirectoryOpts{Description: "Tool scratch"})
	if h == nil || h.agent != a || h.path != "tmp" {
		t.Fatalf("tmp handle = %+v", h)
	}
}
