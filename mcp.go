package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/airlockrun/goai/tool"
)

// MCPHandle is a compile-time binding to a registered MCP server.
// Returned by RegisterMCP, used to call tools and build tool sets.
type MCPHandle struct {
	slug  string
	agent *Agent
}

// CallTool calls a tool on this MCP server via Airlock's proxy.
func (h *MCPHandle) CallTool(ctx context.Context, toolName string, args map[string]any) (*MCPToolCallResponse, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("MCPHandle.CallTool: marshal args: %w", err)
	}
	req := MCPToolCallRequest{
		Tool:      toolName,
		Arguments: argsJSON,
	}
	var resp MCPToolCallResponse
	if err := h.agent.client.doJSON(ctx, "POST", "/api/agent/mcp/"+h.slug+"/tools/call", req, &resp); err != nil {
		return nil, fmt.Errorf("MCPHandle.CallTool %s/%s: %w", h.slug, toolName, err)
	}
	return &resp, nil
}

// ListTools fetches the current tool schemas from this MCP server via Airlock.
func (h *MCPHandle) ListTools(ctx context.Context) ([]MCPToolSchema, error) {
	var resp struct {
		Tools []MCPToolSchema `json:"tools"`
	}
	if err := h.agent.client.doJSON(ctx, "GET", "/api/agent/mcp/"+h.slug+"/tools", nil, &resp); err != nil {
		return nil, fmt.Errorf("MCPHandle.ListTools %s: %w", h.slug, err)
	}
	return resp.Tools, nil
}

// Tools returns a goai tool.Set for this MCP server's discovered tools.
// Use this to pass MCP tools to a stream.StreamText call.
func (h *MCPHandle) Tools(ctx context.Context) (tool.Set, error) {
	schemas, err := h.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	return h.buildToolSet(schemas), nil
}

// Slug returns the MCP server slug.
func (h *MCPHandle) Slug() string { return h.slug }

// buildToolSet creates a tool.Set from MCP tool schemas.
func (h *MCPHandle) buildToolSet(schemas []MCPToolSchema) tool.Set {
	ts := make(tool.Set)
	for _, s := range schemas {
		name := s.Name
		schema := s.InputSchema
		desc := s.Description

		ts[name] = tool.Tool{
			Name:        name,
			Description: desc,
			InputSchema: schema,
			Execute: func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
				var args map[string]any
				if len(input) > 0 {
					json.Unmarshal(input, &args)
				}
				resp, err := h.CallTool(ctx, name, args)
				if err != nil {
					if ae, ok := IsAuthRequired(err); ok {
						return tool.Result{
							Output: fmt.Sprintf("MCP server %q requires authorization. User must connect at: %s", h.slug, ae.AuthURL),
						}, nil
					}
					return tool.Result{Output: "MCP error: " + err.Error()}, nil
				}
				if resp.IsError {
					text := "MCP tool error"
					if len(resp.Content) > 0 {
						text = resp.Content[0].Text
					}
					return tool.Result{Output: "Error: " + text}, nil
				}
				var out strings.Builder
				for _, c := range resp.Content {
					if c.Type == "text" {
						out.WriteString(c.Text)
					}
				}
				return tool.Result{Output: out.String()}, nil
			},
		}
	}
	return ts
}
