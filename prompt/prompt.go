// Package prompt renders the agent's system prompt at run time from
// the agent's live registrations (tools, connections, MCPs, topics,
// webhooks, crons, routes) plus the small slice of platform data
// Airlock supplies at sync (dashboard/route URLs, the sibling address
// book). The same template was previously rendered server-side in
// airlock/prompt; moving it here lets the per-run caller filter
// (caller access level, per-user sibling visibility) take effect
// without exploding the on-wire payload into N variants.
package prompt

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"text/template"

	"github.com/airlockrun/agentsdk/tsrender"
	"github.com/google/uuid"
)

//go:embed agent.tmpl
var agentPromptTmpl string

var agentTmpl = template.Must(template.New("agent").Funcs(template.FuncMap{
	"renderTools":             renderToolsFunc,
	"renderMCPNamespace":      renderMCPNamespaceFunc,
	"renderSiblingNamespace":  renderSiblingNamespaceFunc,
}).Parse(agentPromptTmpl))

// AgentData is the template input. Lists are pre-filtered for the
// caller's access level by Render before the template runs; the
// template itself is access-blind.
type AgentData struct {
	AgentDashboardURL string
	AgentRouteURL     string
	Tools             []ToolInfo
	Connections       []ConnInfo
	Topics            []TopicInfo
	Webhooks          []WebhookInfo
	Crons             []CronInfo
	Routes            []RouteInfo
	MCPServers        []MCPServerStatus
	Siblings          []SiblingInfo
}

// ToolInfo carries the hydrated tool record for prompt rendering.
type ToolInfo struct {
	Name         string
	Description  string
	LLMHint      string
	Access       string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// ConnInfo / TopicInfo / WebhookInfo / CronInfo / RouteInfo are the
// flat shapes the template iterates. agent.RenderSystemPrompt builds
// them from the agent's in-memory registrations.

type ConnInfo struct {
	Slug        string
	Name        string
	Description string
	LLMHint     string
	BaseURL     string
	Access      string
}

type TopicInfo struct {
	Slug        string
	Description string
	LLMHint     string
	Access      string
}

type WebhookInfo struct {
	Path        string
	Description string
}

type CronInfo struct {
	Name        string
	Schedule    string
	Description string
}

type RouteInfo struct {
	Method      string
	Path        string
	Access      string
	Description string
}

// MCPServerStatus carries the per-server status line + (when authorized)
// the discovered tool schemas the template renders into a typed
// `declare const mcp_{slug}: {...}` block.
type MCPServerStatus struct {
	Slug   string
	Name   string
	Status string // "connected, 5 tools" or "requires authentication"
	Access string
	Tools  []ToolInfo
}

// SiblingInfo carries one sibling agent record for prompt rendering.
// ID is the canonical, rename-safe identifier; Slug is the
// human-readable binding name (`agent_<slug>`). Tools is the
// sibling's published tool schemas (synced server-side).
type SiblingInfo struct {
	ID          uuid.UUID
	Slug        string
	Name        string
	Description string
	Tools       []ToolInfo
}

func renderToolsFunc(tools []ToolInfo) string {
	items := make([]tsrender.ToolRender, len(tools))
	for i, t := range tools {
		items[i] = tsrender.ToolRender{
			Name:         t.Name,
			Description:  t.Description,
			LLMHint:      t.LLMHint,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
		}
	}
	return tsrender.RenderToolDecls(items)
}

func renderMCPNamespaceFunc(server MCPServerStatus) string {
	if len(server.Tools) == 0 {
		return ""
	}
	tools := make([]tsrender.MCPToolRender, len(server.Tools))
	for i, t := range server.Tools {
		tools[i] = tsrender.MCPToolRender{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return tsrender.RenderMCPNamespace(server.Slug, tools)
}

// renderSiblingNamespaceFunc produces the typed `declare const
// agent_{slug}: {...}` block. The MCP namespace renderer already
// emits the right shape — sibling tools and MCP tools are both
// "named tool with input schema returning JSON" — so we reuse it
// with the agent_ prefix. The built-in `prompt` meta-tool is added
// at the end with a hard-coded shape (no schema diff per sibling).
func renderSiblingNamespaceFunc(s SiblingInfo) string {
	tools := make([]tsrender.MCPToolRender, 0, len(s.Tools)+1)
	for _, t := range s.Tools {
		tools = append(tools, tsrender.MCPToolRender{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	// Built-in `prompt` meta-tool — drives the sibling's full LLM loop
	// and returns the final assistant text. Hard-coded shape because
	// every sibling exposes the same method.
	tools = append(tools, tsrender.MCPToolRender{
		Name:        "prompt",
		Description: "Send a natural-language prompt to this agent. The agent runs its own LLM loop and returns the final assistant message. Use this for open-ended delegation; use the typed tools above for narrow tool-call shapes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"User-facing message to send."},"conversationId":{"type":"string","description":"Optional: continue an existing conversation. Omit to start fresh."}},"required":["message"]}`),
	})
	return tsrender.RenderMCPNamespace("agent_"+s.Slug, tools)
}

// accessRank totally orders the three access levels.
func accessRank(s string) int {
	switch s {
	case "admin":
		return 3
	case "user":
		return 2
	case "public":
		return 1
	case "":
		return 2
	}
	return -1
}

// callerSatisfies reports whether a caller at level caller may see
// something registered at level required. Empty caller is treated as
// admin (unfiltered).
func callerSatisfies(caller, required string) bool {
	if caller == "" {
		return true
	}
	return accessRank(caller) >= accessRank(required)
}

// Render renders the system prompt for the given caller access. Pass
// caller as "admin", "user", or "public"; empty means unfiltered
// admin variant (used by tests).
func Render(data AgentData, caller string) (string, error) {
	if caller != "" {
		data = filterAgentData(data, caller)
	}
	var buf bytes.Buffer
	if err := agentTmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func filterAgentData(data AgentData, caller string) AgentData {
	out := data
	out.Tools = filterTools(data.Tools, caller)
	out.Connections = filterConns(data.Connections, caller)
	out.Topics = filterTopics(data.Topics, caller)
	out.Routes = filterRoutes(data.Routes, caller)
	out.MCPServers = filterMCPs(data.MCPServers, caller)
	// Siblings are not access-filtered by caller tier — visibility for
	// siblings is per-user (visible-set intersection) and that filter
	// is applied by the agent BEFORE calling Render. The caller here
	// receives whatever Siblings were passed in.
	return out
}

func filterTools(in []ToolInfo, caller string) []ToolInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]ToolInfo, 0, len(in))
	for _, t := range in {
		if callerSatisfies(caller, t.Access) {
			out = append(out, t)
		}
	}
	return out
}

func filterConns(in []ConnInfo, caller string) []ConnInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]ConnInfo, 0, len(in))
	for _, c := range in {
		if callerSatisfies(caller, c.Access) {
			out = append(out, c)
		}
	}
	return out
}

func filterTopics(in []TopicInfo, caller string) []TopicInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]TopicInfo, 0, len(in))
	for _, t := range in {
		if callerSatisfies(caller, t.Access) {
			out = append(out, t)
		}
	}
	return out
}

func filterRoutes(in []RouteInfo, caller string) []RouteInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]RouteInfo, 0, len(in))
	for _, r := range in {
		if callerSatisfies(caller, r.Access) {
			out = append(out, r)
		}
	}
	return out
}

func filterMCPs(in []MCPServerStatus, caller string) []MCPServerStatus {
	if len(in) == 0 {
		return in
	}
	out := make([]MCPServerStatus, 0, len(in))
	for _, m := range in {
		if callerSatisfies(caller, m.Access) {
			out = append(out, m)
		}
	}
	return out
}
