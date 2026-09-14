package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

const instructions = `You are an application-owned task agent. Finish by calling complete, never by prose alone.
complete accepts either {kind:"output",output:<typed result>} or {kind:"needs_input",question:"..."}; both finish this run. There is no yield state.
Agent controls are direct model tools, not JavaScript functions. run_js executes synchronously. Calls in one response execute in order; successful complete stops the batch.
spawn tools return durable call IDs and session IDs. Use get_calls to inspect, wait_calls to wait for any/all, cancel_calls to cancel, and continue_agent to start a new run in a completed child's session. Resolve active children before complete.
Waiting releases this worker and destroys JavaScript state. Every restart creates a fresh realm. Use returned transcript data, never retained JavaScript heap state, to continue work.
An interrupted tool outcome marked unknown is not proof of failure. Do not repeat its side effects automatically. Use safe reads or complete with needs_input if a decision requires clarification.
No human is present for interactive approval. Host capability policy authorizes application-owned work; request_confirmation does not create a human approval flow.`

func tools(in Input, js tool.Tool) (tool.Set, map[string]string, error) {
	set := tool.Set{js.Name: js}
	children := make(map[string]wire.AgentDefinition, len(in.Subagents))
	for _, d := range in.Subagents {
		if _, exists := children[d.Slug]; exists {
			return nil, nil, fmt.Errorf("duplicate child contract %q", d.Slug)
		}
		children[d.Slug] = d
	}
	spawns := make(map[string]string, len(in.Definition.Subagents))
	for _, slug := range in.Definition.Subagents {
		d, ok := children[slug]
		if !ok || !json.Valid(d.InputSchema) {
			return nil, nil, fmt.Errorf("missing or invalid child contract %q", slug)
		}
		if err := checkSchema(d.InputSchema); err != nil {
			return nil, nil, fmt.Errorf("child %q input schema: %w", slug, err)
		}
		name := "spawn_" + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
				return r
			}
			return '_'
		}, slug)
		if _, exists := set[name]; exists {
			return nil, nil, fmt.Errorf("duplicate spawn tool %q", name)
		}
		set[name] = tool.Tool{Name: name, Description: d.Description + "\nStart a child and return its durable call information.", InputSchema: d.InputSchema}
		spawns[name] = slug
	}
	if len(children) != len(spawns) {
		return nil, nil, errors.New("Subagents must exactly match Definition.Subagents")
	}
	set["continue_agent"] = tool.New("continue_agent").Description("Start a new run in a completed child session; return durable call information.").SchemaFromStruct(struct {
		SessionID string `json:"sessionId"`
		Prompt    string `json:"prompt"`
	}{}).Build()
	set["get_calls"] = tool.New("get_calls").Description("Inspect owned child calls without waiting.").SchemaFromStruct(wire.AgentCallsRequest{}).Build()
	set["cancel_calls"] = tool.New("cancel_calls").Description("Cancel owned child calls; return their call information.").SchemaFromStruct(wire.AgentCallsRequest{}).Build()
	set["wait_calls"] = tool.New("wait_calls").Description("Wait for any or all selected child calls. Parking retains this exact call and its original deadline; timeout does not cancel children.").Schema(json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true},"mode":{"type":"string","enum":["any","all"]},"timeoutMs":{"type":"integer","minimum":0}},"required":["ids","mode"],"additionalProperties":false}`)).Build()
	// Namespace local references so recursive output schemas still address the
	// output contract, not the enclosing completion envelope.
	if err := checkSchema(in.Definition.OutputSchema); err != nil {
		return nil, nil, fmt.Errorf("output schema: %w", err)
	}
	var output any
	d := json.NewDecoder(bytes.NewReader(in.Definition.OutputSchema))
	d.UseNumber()
	if err := d.Decode(&output); err != nil {
		return nil, nil, fmt.Errorf("output schema: %w", err)
	}
	var relocate func(any)
	relocate = func(v any) {
		if x, ok := v.(map[string]any); ok {
			for k, child := range x {
				switch k {
				case "$ref":
					x[k] = "#/$defs/agentOutput" + strings.TrimPrefix(child.(string), "#")
				case "properties", "$defs", "definitions":
					for _, schema := range child.(map[string]any) {
						relocate(schema)
					}
				case "items", "additionalProperties", "not":
					relocate(child)
				case "anyOf", "allOf", "oneOf":
					for _, schema := range child.([]any) {
						relocate(schema)
					}
				}
			}
		}
	}
	relocate(output)
	encoded, err := json.Marshal(map[string]any{
		"type": "object", "$defs": map[string]any{"agentOutput": output},
		"oneOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "output"}, "output": map[string]any{"$ref": "#/$defs/agentOutput"}}, "required": []string{"kind", "output"}, "additionalProperties": false},
			map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"const": "needs_input"}, "question": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"kind", "question"}, "additionalProperties": false},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	set["complete"] = tool.New("complete").Description("Finish this run with a typed output or a question requiring input. Active children can prevent completion. No later calls execute after success.").Schema(encoded).Build()
	return set, spawns, nil
}

func decode(input json.RawMessage, dst any) error {
	if len(input) == 0 || bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		return errors.New("object argument is required")
	}
	d := json.NewDecoder(bytes.NewReader(input))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func dispatch(ctx context.Context, in Input, set tool.Set, spawns map[string]string, call Call) (tool.Result, *wire.AgentReply, error) {
	t, ok := set[call.Name]
	if !ok {
		return tool.Result{}, nil, fmt.Errorf("unknown tool %q", call.Name)
	}
	if err := validateJSON(t.InputSchema, call.Input); err != nil {
		return tool.Result{}, nil, fmt.Errorf("%s input: %w", call.Name, err)
	}
	if call.Name == "run_js" {
		r, err := t.Execute(ctx, call.Input, tool.CallOptions{ToolCallID: call.ID, AbortSignal: ctx})
		return r, nil, err
	}
	var result any
	var err error
	if slug, ok := spawns[call.Name]; ok {
		result, err = in.Controller.Spawn(ctx, call.ID, slug, call.Input)
	} else {
		switch call.Name {
		case "continue_agent":
			var args struct {
				SessionID string `json:"sessionId"`
				Prompt    string `json:"prompt"`
			}
			if err := decode(call.Input, &args); err != nil {
				return tool.Result{}, nil, err
			}
			if strings.TrimSpace(args.SessionID) == "" || strings.TrimSpace(args.Prompt) == "" {
				return tool.Result{}, nil, errors.New("sessionId and prompt are required")
			}
			result, err = in.Controller.Continue(ctx, call.ID, args.SessionID, args.Prompt)
		case "get_calls", "cancel_calls", "wait_calls":
			var args wire.AgentWaitRequest
			if call.Name == "wait_calls" {
				if err := decode(call.Input, &args); err != nil {
					return tool.Result{}, nil, err
				}
				if args.Mode != "any" && args.Mode != "all" || args.TimeoutMS != nil && *args.TimeoutMS < 0 {
					return tool.Result{}, nil, errors.New("wait requires mode any/all and nonnegative timeoutMs")
				}
			} else {
				var calls wire.AgentCallsRequest
				if err := decode(call.Input, &calls); err != nil {
					return tool.Result{}, nil, err
				}
				args.IDs = calls.IDs
			}
			seen := map[string]bool{}
			for _, id := range args.IDs {
				if strings.TrimSpace(id) == "" || seen[id] {
					return tool.Result{}, nil, errors.New("call IDs must be nonempty and unique")
				}
				seen[id] = true
			}
			if len(seen) == 0 {
				return tool.Result{}, nil, errors.New("at least one call ID is required")
			}
			switch call.Name {
			case "get_calls":
				result, err = in.Controller.Get(ctx, args.IDs)
			case "cancel_calls":
				result, err = in.Controller.Cancel(ctx, args.IDs)
			case "wait_calls":
				result, err = in.Controller.Wait(ctx, call.ID, args)
			}
		case "complete":
			var reply wire.AgentReply
			if err := decode(call.Input, &reply); err != nil {
				return tool.Result{}, nil, err
			}
			switch reply.Kind {
			case "output":
				if reply.Question != "" {
					return tool.Result{}, nil, errors.New("output reply cannot contain a question")
				}
			case "needs_input":
				if len(reply.Output) != 0 || strings.TrimSpace(reply.Question) == "" {
					return tool.Result{}, nil, errors.New("needs_input requires a question and no output")
				}
			default:
				return tool.Result{}, nil, errors.New("complete kind must be output or needs_input")
			}
			if err := in.Controller.Complete(ctx, reply); err != nil {
				return tool.Result{}, nil, err
			}
			return tool.Result{Output: `{"completed":true}`}, &reply, nil
		}
	}
	if err != nil {
		return tool.Result{}, nil, err
	}
	encoded, err := json.Marshal(result)
	return tool.Result{Output: string(encoded)}, nil, err
}
