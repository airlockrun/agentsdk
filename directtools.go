package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"

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
// LLM tool here, filtered by the run's caller access. Names mirror the
// JS forms but flatten dotted namespaces to underscores so the LLM sees
// deterministic strings (e.g. `conn_slack.requestJSON` → `conn_slack_request_json`).
//
// The Execute bodies invoke the same Go helpers the JS bindings call —
// no logic duplication. Schemas come from per-binding input structs
// (fixed primitives) or from sync-cached schemas (RegisteredTools, MCP
// tools, sibling tools).
//
// promptAgent is added by buildSolTools after the direct-tools set is
// assembled so the open-ended delegation primitive lives in both modes.
func buildDirectTools(agent *Agent, run *run, supportedModalities []string) tool.Set {
	ts := tool.Set{}
	// Order mirrors newVM (vm.go): registered tools first, then built-ins,
	// then namespaced bindings. Last write wins on name collisions — so an
	// author's tool named `fileRead` is silently shadowed by the built-in
	// `fileRead` in *both* modes (JS via vm.Set last-write, direct via
	// this map assignment). Keeping the behaviour identical means there's
	// no surprise where a registered tool works in run_js but disappears
	// in direct mode (or vice versa). Names are camelCase for built-ins
	// and `{prefix}_{slug}_{method}` for namespaced bindings — the same
	// strings the JS bindings use.
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
		if !accessSatisfies(run.callerAccess, rt.Access) {
			continue
		}
		ts[name] = buildRegisteredTool(rt, run)
	}
}

func buildRegisteredTool(rt *registeredTool, run *run) tool.Tool {
	desc := rt.Description
	if rt.LLMHint != "" {
		desc = desc + " [" + rt.LLMHint + "]"
	}
	def := tool.New(rt.Name).
		Description(desc).
		Schema(rt.InputSchema.MustJSON()).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			ctx = contextWithRun(run.checkedCtx(), run)
			out, err := rt.Execute(ctx, input)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		})
	for _, ex := range rt.InputExamples {
		def = def.InputExample(ex)
	}
	return def.Build()
}

