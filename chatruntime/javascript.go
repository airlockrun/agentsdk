package chatruntime

import (
	"context"
	"errors"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/goai/tool"
)

// JavaScriptTool builds synchronous run_js for an application-owned run. The
// host preauthorizes the catalog and enforces policy in Backend; there is no
// interactive confirmation. Call close on every exit, including parking. The
// factory allocates lazily; only catalog capabilities become JavaScript bindings.
func JavaScriptTool(ctx context.Context, catalog []capability.Definition, backend Backend, factory ExecutorFactory) (js tool.Tool, close func() error, err error) {
	set, close, err := JavaScriptTools(ctx, catalog, backend, factory)
	if err != nil {
		return tool.Tool{}, nil, err
	}
	return set["run_js"], close, nil
}

// JavaScriptTools builds run_js and, when file reads are authorized,
// run_js_file over one lazily allocated realm. Close it on every attempt exit.
func JavaScriptTools(ctx context.Context, catalog []capability.Definition, backend Backend, factory ExecutorFactory) (tool.Set, func() error, error) {
	if backend == nil || factory == nil {
		return nil, nil, errors.New("chatruntime: Backend and ExecutorFactory are required")
	}
	if err := validateCapabilities(catalog); err != nil {
		return nil, nil, err
	}
	r := &runtime{ctx: ctx, input: Input{Capabilities: catalog, Backend: backend, ExecutorFactory: factory, AutoConfirm: true}}
	set := r.tools()
	js := set["run_js"]
	js.Description = "Execute an async JavaScript function body synchronously. Await every capability call and explicitly return durable results. Supply a short description. Agent-control tools are not JavaScript bindings."
	set["run_js"] = js
	return set, func() error {
		if r.executor == nil {
			return nil
		}
		return r.executor.Close()
	}, nil
}
