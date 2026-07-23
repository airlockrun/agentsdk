package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/airlockrun/agentsdk/internal/binding"
	"github.com/airlockrun/goai/tool"
)

// directTool wraps a typed Go function `fn(ctx, in I) (O, error)` as an LLM
// tool. The schema reflects from `I`, JSON input unmarshals into `I`, and
// the output marshals as JSON. Used by addBuiltinTools and the per-instance
// loops in directtools_dynamic.go so each builtin/namespaced binding is a
// one-line definition rather than a 20-line builder.
//
// Use directToolRaw when the output is already a string (e.g. fileRead's
// text content) — it skips the json.Marshal and avoids double-quoting.
func directTool[I, O any](name, desc string, fn func(context.Context, I) (O, error)) tool.Tool {
	var zero I
	return tool.New(name).
		Description(desc).
		SchemaFromStruct(zero).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in I
			if len(input) > 0 && string(input) != "null" {
				if err := json.Unmarshal(input, &in); err != nil {
					return tool.Result{}, fmt.Errorf("%s: invalid input: %w", name, err)
				}
			}
			out, err := fn(ctx, in)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := json.Marshal(out)
			if err != nil {
				return tool.Result{}, fmt.Errorf("%s: encode output: %w", name, err)
			}
			return tool.Result{Output: string(b)}, nil
		}).Build()
}

// directToolRaw is the string-output variant of directTool. The function
// returns a string, which becomes Output verbatim (no JSON wrapping). Used
// where the LLM should see plain text — fileRead, fileGrep, log-like
// readouts — not a JSON-encoded string.
func directToolRaw[I any](name, desc string, fn func(context.Context, I) (string, error)) tool.Tool {
	var zero I
	return tool.New(name).
		Description(desc).
		SchemaFromStruct(zero).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in I
			if len(input) > 0 && string(input) != "null" {
				if err := json.Unmarshal(input, &in); err != nil {
					return tool.Result{}, fmt.Errorf("%s: invalid input: %w", name, err)
				}
			}
			out, err := fn(ctx, in)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

// buildDirectTools builds the tool.Set served when run.directTools is true.
// Each capability the goja VM exposes as a JS binding becomes its own typed
// LLM tool here, filtered by the run's caller access.
//
// The Execute bodies invoke the same Go helpers the JS bindings call —
// no logic duplication. Schemas come from per-binding input structs
// (fixed primitives) or from sync-cached schemas (RegisteredTools, MCP
// tools, sibling tools).
//
// promptAgent is added by buildSolTools after the direct-tools set is
// assembled so the open-ended delegation primitive lives in both modes.
func buildDirectTools(agent *Agent, run *run) tool.Set {
	ts := tool.Set{}
	addRegisteredTools(ts, agent, run)
	addBuiltinTools(ts, agent, run)
	addNamespacedTools(ts, agent, run)
	return ts
}

// addRegisteredTools wraps every agent.RegisterTool declaration whose
// Access tier is reachable by this run. The author-supplied input schema
// + Execute closure carry over verbatim — RegisteredTool was already a
// typed, schema-bearing capability; in JS mode it just happened to be
// dispatched from inside the goja VM.
func addRegisteredTools(ts tool.Set, agent *Agent, run *run) {
	for name, rt := range agent.tools {
		if !accessSatisfies(run.callerAccess, rt.access) {
			continue
		}
		addDirectBinding(ts, binding.Local(binding.Tool, "", name), buildRegisteredTool(rt, run))
	}
}

func addDirectBinding(ts tool.Set, path binding.Path, t tool.Tool) {
	name := path.Direct()
	if _, exists := ts[name]; exists {
		panic("agentsdk: duplicate direct capability binding: " + name)
	}
	t.Name = name
	ts[name] = t
}

// wrapToolWithRun returns a copy of t whose Execute runs under the run-scoped
// context (caller, cancellation, run value), so agent-facility tools — storage,
// events, sub-prompts — resolve the agent and run. Provider/no-execute tools
// pass through unchanged. Shared by registered tools and the agent's
// GenerateText/StreamText sub-calls.
func wrapToolWithRun(t tool.Tool, run *run) tool.Tool {
	if t.Execute == nil {
		return t
	}
	inner := t.Execute
	t.Execute = func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		return inner(contextWithRun(run.checkedCtx(), run), input, opts)
	}
	return t
}

func buildRegisteredTool(rt *registeredTool, run *run) tool.Tool {
	t := wrapToolWithRun(rt.Tool, run)
	if rt.llmHint != "" {
		t.Description = t.Description + " [" + rt.llmHint + "]"
	}
	return t
}
