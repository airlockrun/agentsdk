package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// SiblingHandle invokes tools on a sibling agent via airlock's MCP server
// endpoint. The wire path is identical to what an external MCP client
// (Claude Desktop, VS Code) would use — JSON-RPC `tools/call` to
// /api/agent/{siblingID}/mcp — but auth is this agent's own bearer
// token plus an X-Run-ID header that tells airlock which caller-side run
// the call belongs to.
//
// File arguments and results are translated at the airlock boundary:
// FilePath args declared in the sibling's tool input schema are copied
// cross-bucket into the sibling's __a2a/{callerRunID}/ namespace before
// dispatch, and FilePath results are copied back into this agent's
// a2a/<sibling-slug>/ namespace. The handle just speaks JSON-RPC; the
// translation is invisible.
type SiblingHandle struct {
	slug    string
	agentID uuid.UUID
	agent   *Agent
}

// CallTool invokes a named tool on the sibling. callerRunID is the
// current run's ID (X-Run-ID header) — required so airlock can resolve
// the caller's identity for permissions + key __a2a/{runID}/ blobs.
func (h *SiblingHandle) CallTool(ctx context.Context, callerRunID, toolName string, args any) (any, error) {
	argsJSON, err := encodeMCPArgs(args)
	if err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool: encode args: %w", err)
	}

	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      uuid.NewString(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": argsJSON,
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool: marshal envelope: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		h.agent.client.baseURL+"/api/agent/"+h.agentID.String()+"/mcp",
		bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.agent.client.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Run-ID", callerRunID)

	resp, err := h.agent.client.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool agent_%s.%s: %w", h.slug, toolName, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("SiblingHandle.CallTool agent_%s.%s: status %d: %s", h.slug, toolName, resp.StatusCode, string(raw))
	}

	// JSON-RPC response envelope.
	var rpcResp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("SiblingHandle.CallTool agent_%s.%s: decode envelope: %w", h.slug, toolName, err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("agent_%s.%s: %s (code %d)", h.slug, toolName, rpcResp.Error.Message, rpcResp.Error.Code)
	}

	// MCP tool-call result: {content: [...], isError?: bool}.
	var toolResult struct {
		Content []struct {
			Type string          `json:"type"`
			Text string          `json:"text,omitempty"`
			URI  string          `json:"uri,omitempty"`
			Data json.RawMessage `json:"data,omitempty"`
		} `json:"content"`
		IsError bool `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(rpcResp.Result, &toolResult); err != nil {
		return nil, fmt.Errorf("agent_%s.%s: decode result: %w", h.slug, toolName, err)
	}

	// Concatenate text content. The A2A boundary materializer rewrites
	// FilePath results in-place inside the text block, so by the time
	// we're parsing here the path strings are already in this agent's
	// own a2a/<slug>/ namespace. resource_link blocks are reserved for
	// external MCP clients; A2A skips them on the airlock side, but if
	// any leak through we record their URIs as a fallback so the LLM
	// at least sees them.
	var textBuf bytes.Buffer
	for _, c := range toolResult.Content {
		switch c.Type {
		case "text":
			textBuf.WriteString(c.Text)
		case "resource_link":
			textBuf.WriteString(c.URI)
		}
	}
	text := textBuf.String()
	if toolResult.IsError {
		return nil, fmt.Errorf("agent_%s.%s: %s", h.slug, toolName, text)
	}

	// Tool bodies usually return JSON; decode if possible so JS sees a
	// real object. Otherwise return the raw string.
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		return parsed, nil
	}
	return text, nil
}
