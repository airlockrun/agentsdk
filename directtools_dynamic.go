package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/airlockrun/agentsdk/tsrender"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

// Per-instance direct-tool factories: connections, exec endpoints,
// topics, MCP servers, sibling agents. Each registered instance fans
// out into one or more typed tools whose names are flat underscores
// (e.g. `mcp_github_search_repos`). Schemas for MCP/sibling tools come
// from sync; everything else is a fixed Go input struct.

// addNamespacedTools registers every namespaced binding the run's
// caller is allowed to see. Mirrors the per-namespace loops in
// newVM (vm.go) but emits standalone LLM tools instead of goja
// object methods.
func addNamespacedTools(ts tool.Set, agent *Agent, run *run) {
	addConnectionTools(ts, agent, run)
	addExecTools(ts, agent, run)
	addTopicTools(ts, agent, run)
	addMCPTools(ts, agent, run)
	addSiblingTools(ts, agent, run)
}

// --- connections ---

type connRequestInput struct {
	Method  string            `json:"method" jsonschema:"description=HTTP method (GET, POST, ...)."`
	Path    string            `json:"path" jsonschema:"description=Path appended to the connection's base URL."`
	Body    any               `json:"body,omitempty" jsonschema:"description=Request body. For request_json, an object/array is JSON-encoded; for the raw request tool, pass a string."`
	Headers map[string]string `json:"headers,omitempty"`
}

func addConnectionTools(ts tool.Set, agent *Agent, run *run) {
	for slug, conn := range agent.auths {
		if !accessSatisfies(run.callerAccess, conn.Access) {
			continue
		}
		handle := &ConnectionHandle{slug: slug, agent: run.agent}
		slugCap := slug
		desc := conn.Description
		if desc == "" {
			desc = "Service connection: " + slug
		}
		ts["conn_"+slug+"_request"] = buildConnRequestTool("conn_"+slug+"_request", desc+" — raw response (string body).", slugCap, handle, run, false)
		ts["conn_"+slug+"_request_json"] = buildConnRequestTool("conn_"+slug+"_request_json", desc+" — JSON response (auto-parsed into `data`).", slugCap, handle, run, true)
	}
}

func buildConnRequestTool(name, desc, connSlug string, handle *ConnectionHandle, run *run, jsonMode bool) tool.Tool {
	return tool.New(name).
		Description(desc).
		SchemaFromStruct(connRequestInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in connRequestInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Method == "" {
				return tool.Result{}, errors.New("method is required")
			}
			body := in.Body
			if !jsonMode {
				// Raw mode expects a string body; coerce non-string for
				// parity with the JS binding's `body = call.Argument(2).String()`.
				if s, ok := body.(string); ok {
					body = s
				} else if body != nil {
					body = fmt.Sprintf("%v", body)
				}
			}
			res, err := connRequestExec(run.ctx, run.agent, handle, connSlug, RequestOpts{
				Method: in.Method, Path: in.Path, Body: body, Headers: in.Headers,
			})
			if err != nil {
				return tool.Result{}, connectionErrorForJS(connSlug, err)
			}
			out := map[string]any{
				"status":      res.Status,
				"contentType": res.ContentType,
				"size":        res.Size,
			}
			mode := "raw"
			if jsonMode {
				mode = "json"
			}
			if res.Spilled {
				out["bodyPreview"] = string(res.BodyPreview)
				out["bodySavedTo"] = res.SavedTo
				out["note"] = connSpilledNote(res.Size, res.SavedTo, mode)
				return jsonResult(out)
			}
			if jsonMode {
				if len(res.Inline) == 0 {
					out["data"] = nil
				} else {
					var parsed any
					if err := json.Unmarshal(res.Inline, &parsed); err != nil {
						return tool.Result{}, fmt.Errorf("decode JSON response: %w", err)
					}
					out["data"] = parsed
				}
			} else {
				out["body"] = string(res.Inline)
			}
			return jsonResult(out)
		}).Build()
}

// --- exec endpoints ---

type execRunInput struct {
	Command   string   `json:"command" jsonschema:"description=Command string. Shell features (pipes, redirection) live here."`
	Args      []string `json:"args,omitempty" jsonschema:"description=Positional args appended to command. Quoted before being sent to the remote shell."`
	Stdin     string   `json:"stdin,omitempty"`
	TimeoutMs int64    `json:"timeoutMs,omitempty"`
}

func addExecTools(ts tool.Set, agent *Agent, run *run) {
	for slug, ep := range agent.execEndpoints {
		if !accessSatisfies(run.callerAccess, ep.Access) {
			continue
		}
		handle := &ExecHandle{slug: slug, agent: run.agent}
		epSlug := slug
		desc := ep.Description
		if desc == "" {
			desc = "Run a command on " + slug
		}
		desc += " — returns {stdout, stderr, exitCode, durationMs}. Non-zero exitCode is not an error; inspect it yourself. Stdout/stderr over 20 MiB spill to tmp/."
		ts["exec_"+slug+"_run"] = tool.New("exec_" + slug + "_run").
			Description(desc).
			SchemaFromStruct(execRunInput{}).
			Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
				var in execRunInput
				if err := json.Unmarshal(input, &in); err != nil {
					return tool.Result{}, err
				}
				cmd := ExecCommand{Command: in.Command, Args: in.Args}
				if in.Stdin != "" {
					cmd.Stdin = []byte(in.Stdin)
				}
				if in.TimeoutMs > 0 {
					cmd.Timeout = time.Duration(in.TimeoutMs) * time.Millisecond
				}
				stream, err := handle.RunStream(run.ctx, cmd)
				if err != nil {
					return tool.Result{}, err
				}
				defer stream.Stdout.Close()
				defer stream.Stderr.Close()

				callID := newCallID()
				prefix := fmt.Sprintf("tmp/exec-%s-%s", epSlug, callID)
				outCh := make(chan spillFields, 1)
				errCh := make(chan spillFields, 1)
				go func() {
					outCh <- spillFor(run.ctx, run.agent, stream.Stdout, prefix+"-stdout.bin")
				}()
				go func() {
					errCh <- spillFor(run.ctx, run.agent, stream.Stderr, prefix+"-stderr.bin")
				}()
				outR := <-outCh
				errR := <-errCh
				exit, waitErr := stream.Wait()
				switch {
				case outR.err != nil:
					return tool.Result{}, outR.err
				case errR.err != nil:
					return tool.Result{}, errR.err
				case waitErr != nil:
					return tool.Result{}, waitErr
				}
				return jsonResult(buildExecRunOutput(outR, errR, exit))
			}).Build()
	}
}

// --- topics ---

type topicEmptyInput struct{}

func addTopicTools(ts tool.Set, agent *Agent, run *run) {
	for slug, topic := range agent.topics {
		if !accessSatisfies(run.callerAccess, topic.Access) {
			continue
		}
		topicSlug := slug
		desc := topic.Description
		if desc == "" {
			desc = "Notification topic: " + slug
		}
		ts["topic_"+slug+"_subscribe"] = directTool("topic_"+slug+"_subscribe",
			desc+" — subscribe the current conversation.",
			func(ctx context.Context, _ topicEmptyInput) (map[string]bool, error) {
				if err := run.subscribeTopic(run.ctx, topicSlug); err != nil {
					return nil, err
				}
				return map[string]bool{"subscribed": true}, nil
			})
		ts["topic_"+slug+"_unsubscribe"] = directTool("topic_"+slug+"_unsubscribe",
			desc+" — unsubscribe the current conversation.",
			func(ctx context.Context, _ topicEmptyInput) (map[string]bool, error) {
				if err := run.unsubscribeTopic(run.ctx, topicSlug); err != nil {
					return nil, err
				}
				return map[string]bool{"unsubscribed": true}, nil
			})
	}
}

// --- MCP servers ---

func addMCPTools(ts tool.Set, agent *Agent, run *run) {
	mcpSchemas := agent.snapshotMCPSchemas()
	for slug, mcp := range agent.mcps {
		if !accessSatisfies(run.callerAccess, mcp.Access) {
			continue
		}
		handle := &MCPHandle{slug: slug, agent: agent}
		schemas := mcpSchemas[slug]
		names := make([]string, len(schemas))
		for i, s := range schemas {
			names[i] = s.Name
		}
		jsNames := tsrender.JSToolNames(names)
		for _, s := range schemas {
			toolName := s.Name
			fullName := "mcp_" + slug + "_" + jsNames[toolName]
			ts[fullName] = buildMCPTool(fullName, s, handle, toolName, run)
		}
	}
}

func buildMCPTool(toolName string, schema MCPToolSchema, handle *MCPHandle, mcpToolName string, run *run) tool.Tool {
	desc := schema.Description
	if desc == "" {
		desc = mcpToolName
	}
	def := tool.New(toolName).Description(desc)
	if len(schema.InputSchema) > 0 && string(schema.InputSchema) != "null" {
		def = def.Schema(json.RawMessage(schema.InputSchema))
	} else {
		def = def.Schema(json.RawMessage(`{"type":"object"}`))
	}
	def = def.Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		var args any
		if len(input) > 0 && string(input) != "null" {
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Result{}, err
			}
		}
		result, err := handle.CallTool(run.ctx, mcpToolName, args)
		if err != nil {
			return tool.Result{}, err
		}
		return jsonResult(result)
	})
	return def.Build()
}

// --- siblings (typed tools only; promptAgent stays in buildSolTools) ---

func addSiblingTools(ts tool.Set, agent *Agent, run *run) {
	if len(run.visibleSiblings) == 0 {
		return
	}
	visible := make(map[uuid.UUID]struct{}, len(run.visibleSiblings))
	for _, id := range run.visibleSiblings {
		visible[id] = struct{}{}
	}
	agent.syncMu.RLock()
	siblings := agent.promptData.Siblings
	agent.syncMu.RUnlock()
	for _, s := range siblings {
		if _, ok := visible[s.ID]; !ok {
			continue
		}
		handle := &SiblingHandle{slug: s.Slug, agentID: s.ID, agent: agent}
		names := make([]string, len(s.Tools))
		for i, t := range s.Tools {
			names[i] = t.Name
		}
		jsNames := tsrender.JSToolNames(names)
		siblingSlug := s.Slug
		for _, t := range s.Tools {
			toolName := t.Name
			fullName := "agent_" + siblingSlug + "_" + jsNames[toolName]
			ts[fullName] = buildSiblingTool(fullName, t, handle, siblingSlug, toolName, run)
		}
	}
}

func buildSiblingTool(toolName string, schema MCPToolSchema, handle *SiblingHandle, siblingSlug, remoteToolName string, run *run) tool.Tool {
	desc := schema.Description
	if desc == "" {
		desc = fmt.Sprintf("Call %s.%s on the %s agent.", siblingSlug, remoteToolName, siblingSlug)
	}
	def := tool.New(toolName).Description(desc)
	if len(schema.InputSchema) > 0 && string(schema.InputSchema) != "null" {
		def = def.Schema(json.RawMessage(schema.InputSchema))
	} else {
		def = def.Schema(json.RawMessage(`{"type":"object"}`))
	}
	def = def.Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		var args any
		if len(input) > 0 && string(input) != "null" {
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Result{}, err
			}
		}
		result, err := handle.CallTool(run.ctx, run.id, remoteToolName, args)
		if err != nil {
			return tool.Result{}, fmt.Errorf("agent_%s.%s: %w", siblingSlug, remoteToolName, err)
		}
		return jsonResult(result)
	})
	return def.Build()
}
