package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// MCPHandle is a compile-time binding to a registered MCP server.
// Returned by RegisterMCP, used to call tools and build tool sets.
type MCPHandle struct {
	slug  string
	agent *Agent
}

// CallTool calls a tool on this MCP server via Airlock's proxy. Args
// encoding mirrors ConnectionHandle.Request:
//
//	nil                           — sent as {} (MCP requires a JSON object)
//	[]byte, string, json.RawMessage — assumed to be valid JSON, sent as-is
//	io.Reader                     — fully read, assumed JSON, sent as-is
//	anything else                 — JSON-marshalled
func (h *MCPHandle) CallTool(ctx context.Context, toolName string, args any) (*MCPToolCallResponse, error) {
	argsJSON, err := encodeMCPArgs(args)
	if err != nil {
		return nil, fmt.Errorf("MCPHandle.CallTool: encode args: %w", err)
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

func encodeMCPArgs(args any) (json.RawMessage, error) {
	switch v := args.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(v) == 0 {
			return json.RawMessage("{}"), nil
		}
		return v, nil
	case []byte:
		if len(v) == 0 {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(v), nil
	case string:
		if v == "" {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(v), nil
	case io.Reader:
		b, err := io.ReadAll(v)
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(b), nil
	default:
		return json.Marshal(v)
	}
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

