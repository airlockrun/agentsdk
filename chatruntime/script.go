package chatruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/goai/tool"
)

// ScriptBackend resolves the current run's inspected capabilities before each
// script. Catalog membership is an exposure ceiling, not execution authority;
// Invoke must independently authorize every operation against live state.
type ScriptBackend interface {
	Backend
	Catalog(context.Context) ([]capability.Definition, error)
}

// ScriptToolInfo returns the provider-facing contract without an executor. MCP
// discovery can describe run_js before allocating any execution context.
func ScriptToolInfo() tool.Tool {
	definition := (&runtime{}).tools()["run_js"]
	definition.Execute = nil
	return definition
}

// NewScriptTool exposes the shared run_js contract without owning a model loop.
// Chat and MCP adapters can both use it. Every call gets a fresh realm, so
// inspection in one script makes its bindings available in the next script.
// Approval applies to the whole script before allocation or execution.
func NewScriptTool(backend ScriptBackend, factory ExecutorFactory, prelude string, autoConfirm bool) tool.Tool {
	return newScriptTool("run_js", ScriptToolInfo(), backend, factory, prelude, autoConfirm)
}

// ScriptFileToolInfo describes saved JS execution without allocating a realm.
func ScriptFileToolInfo() tool.Tool {
	definition := (&runtime{}).scriptTool()
	definition.Execute = nil
	return definition
}

// NewScriptFileTool uses live inspected file-read authority and the same
// isolated execution contract as NewScriptTool, with structured arguments.
func NewScriptFileTool(backend ScriptBackend, factory ExecutorFactory, prelude string, autoConfirm bool) tool.Tool {
	return newScriptTool("run_js_file", ScriptFileToolInfo(), backend, factory, prelude, autoConfirm)
}

func newScriptTool(name string, definition tool.Tool, backend ScriptBackend, factory ExecutorFactory, prelude string, autoConfirm bool) tool.Tool {
	if backend == nil || factory == nil {
		panic("chatruntime: script backend and executor factory are required")
	}
	definition.Description += " Each call has a fresh JavaScript realm; return needed results explicitly."
	definition.Execute = func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (result tool.Result, err error) {
		catalog, err := backend.Catalog(ctx)
		if err != nil {
			return tool.Result{}, err
		}
		if err := validateCapabilities(catalog); err != nil {
			return tool.Result{}, err
		}
		rt := &runtime{ctx: ctx, input: Input{Capabilities: catalog, Backend: backend, ExecutorFactory: factory, AutoConfirm: autoConfirm, JavaScriptPrelude: prelude}}
		defer func() {
			if rt.executor != nil {
				err = errors.Join(err, rt.executor.Close())
			}
		}()
		if name == "run_js_file" {
			return rt.scriptTool().Execute(ctx, input, opts)
		}
		return rt.tools()[name].Execute(ctx, input, opts)
	}
	return definition
}
