// Package prompt renders the agent's system prompt from live registrations
// and caller-specific platform data.
package prompt

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"text/template"

	"github.com/airlockrun/agentsdk/internal/binding"
	"github.com/airlockrun/agentsdk/internal/tsrender"
	"github.com/google/uuid"
)

//go:embed agent.tmpl
var agentPromptTmpl string

var agentTmpl = template.Must(template.New("agent").Funcs(template.FuncMap{
	"renderTools":             renderToolsFunc,
	"renderMCPNamespaces":     renderMCPNamespacesFunc,
	"renderSiblingNamespaces": renderSiblingNamespacesFunc,
}).Parse(agentPromptTmpl))

// AgentData is the template input. Lists are pre-filtered for the
// caller's access level by Render before the template runs, so the
// {{ range }} loops don't have to think about access. The
// CallerAccess field is also exposed so the template CAN branch
// on it when the difference isn't "include/exclude an item" but
// "write a different sentence" — e.g. a public-tier preamble or
// admin-only guidance.
type AgentData struct {
	AgentDashboardURL   string
	AgentRouteURL       string
	CallerAccess        Caller
	Capabilities        Capabilities
	SupportedModalities Modalities
	Tools               []ToolInfo
	Connections         []ConnInfo
	Topics              []TopicInfo
	Webhooks            []WebhookInfo
	Routes              []RouteInfo
	MCPServers          []MCPServerStatus
	Siblings            []SiblingInfo
	Directories         []DirInfo
	ExecEndpoints       []ExecEndpointInfo

	// Per-turn environment, rendered into the <env> block. Each is set
	// explicitly by the dispatch path (never inferred) and omitted from
	// the block when empty.
	Date         string
	Platform     string // web | telegram | discord | a2a
	UserName     string
	UserEmail    string
	Conversation string

	// DirectTools is true when the run exposes each capability as its
	// own typed LLM tool instead of one `run_js` binding. The template
	// branches on this to skip JS-flavoured sections (the TypeScript
	// manifest, `## JavaScript environment`, `var` / `let` guidance, the
	// namespaced binding docs) and emit a short tool-listing block
	// instead. Schemas reach the model via the tool-calling protocol,
	// not the system prompt.
	DirectTools bool
}

// Capabilities mirrors agentsdk.Capabilities — duplicated here to
// avoid an import cycle. buildPromptData copies field-for-field.
type Capabilities struct {
	Vision        bool
	Transcription bool
	Speech        bool
	Embedding     bool
	Image         bool
	Search        bool
}

// Modalities is the list of input types the chat model accepts.
// Wrapped in a type so the template can ask `.SupportedModalities.HasImage`
// instead of doing string comparisons.
type Modalities []string

func (m Modalities) Has(modality string) bool {
	for _, v := range m {
		if v == modality {
			return true
		}
	}
	return false
}

// HasImage / HasPDF / HasAudio / HasVideo are template-friendly
// predicates for the modality strings emitted by sol's
// provider.GetModalities. "text" is always implied so there's no
// matching predicate for it.
func (m Modalities) HasImage() bool { return m.Has("image") }
func (m Modalities) HasPDF() bool   { return m.Has("pdf") }
func (m Modalities) HasAudio() bool { return m.Has("audio") }
func (m Modalities) HasVideo() bool { return m.Has("video") }

// Caller wraps the raw access string with template-friendly
// predicate methods so authors write
// `{{ if .CallerAccess.IsUser }}` instead of `{{ if eq .CallerAccess "user" }}`.
//
// Empty Caller (zero value) is treated as Admin — matches the
// unfiltered "render everything" mode Render uses when caller is
// passed as "".
type Caller string

// IsAdmin reports whether the caller is at the admin tier.
func (c Caller) IsAdmin() bool { return c == "admin" || c == "" }

// IsUser reports whether the caller is exactly at the user tier
// (not admin, not public).
func (c Caller) IsUser() bool { return c == "user" }

// IsPublic reports whether the caller is exactly at the public
// tier — i.e. an anonymous or non-member request.
func (c Caller) IsPublic() bool { return c == "public" }

// AtLeastUser reports user-or-above. Useful for "everyone except
// public" branches.
func (c Caller) AtLeastUser() bool { return accessRank(string(c)) >= accessRank("user") }

// ToolInfo carries the hydrated tool record for prompt rendering.
type ToolInfo struct {
	Name         string
	Description  string
	LLMHint      string
	Access       string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// ConnInfo / TopicInfo / WebhookInfo / RouteInfo are the flat shapes the
// template iterates. agent.RenderSystemPrompt builds them from the agent's
// in-memory registrations.

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

type RouteInfo struct {
	Method      string
	Path        string
	Access      string
	Description string
}

// ExecEndpointInfo carries one registered exec endpoint for the prompt.
// Bind-time access gating happens in vm.go; the prompt block also
// access-filters via filterExecEndpoints below so a non-admin caller
// never sees an admin-only endpoint listed.
type ExecEndpointInfo struct {
	Slug        string
	Description string
	LLMHint     string
	Access      string
}

// DirInfo describes one registered storage directory for the prompt.
// Each cap is shown independently — a caller may, for example, have
// list+read but not write. The template uses these to tell the LLM
// which S3 paths it can touch on this run.
type DirInfo struct {
	Path        string
	Description string
	LLMHint     string
	Read        string
	Write       string
	List        string
	// Scope, when non-empty, indicates the directory is per-context.
	// Values: "user", "conv", "run". The template renders an extra
	// note so the LLM understands paths it returns will include a
	// scope key segment (e.g. `<dir>/user-<id>/...`).
	Scope string
}

// MCPServerStatus carries the per-server status line + (when authorized)
// the discovered tool schemas the template renders into a typed
// nested declaration under the mcp root.
type MCPServerStatus struct {
	Slug   string
	Name   string
	Status string // "connected, 5 tools" or "requires authentication"
	Access string
	// Description is the server-level usage hint the remote MCP server
	// advertised via initialize `instructions`. Empty when unset.
	Description string
	Tools       []ToolInfo
}

// SiblingInfo carries one sibling agent record for prompt rendering.
// ID is the canonical, rename-safe identifier; Slug is the
// human-readable binding name. Tools is the
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
			Path:         binding.Local(binding.Tool, "", t.Name),
			Name:         t.Name,
			Description:  t.Description,
			LLMHint:      t.LLMHint,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
		}
	}
	return tsrender.RenderToolDecls(items)
}

func renderMCPNamespacesFunc(servers []MCPServerStatus) (string, error) {
	namespaces := make([]tsrender.NamespaceRender, 0, len(servers))
	for _, server := range servers {
		names := toolNames(server.Tools)
		paths, err := binding.External(binding.MCP, server.Slug, server.Slug, names)
		if err != nil {
			return "", err
		}
		tools := make([]tsrender.MCPToolRender, len(server.Tools))
		for i, t := range server.Tools {
			tools[i] = tsrender.MCPToolRender{
				Path: paths[t.Name], Name: t.Name,
				Description: t.Description, InputSchema: t.InputSchema,
			}
		}
		if len(tools) > 0 {
			namespaces = append(namespaces, tsrender.NamespaceRender{Namespace: server.Slug, Tools: tools})
		}
	}
	return tsrender.RenderNestedRoot("mcp", namespaces), nil
}

// renderSiblingNamespacesFunc produces typed declarations under the agent root.
func renderSiblingNamespacesFunc(siblings []SiblingInfo) (string, error) {
	namespaces := make([]tsrender.NamespaceRender, 0, len(siblings))
	for _, sibling := range siblings {
		paths, err := binding.External(binding.Agent, sibling.ID.String(), binding.SiblingNamespace(sibling.Slug), toolNames(sibling.Tools))
		if err != nil {
			return "", err
		}
		tools := make([]tsrender.MCPToolRender, len(sibling.Tools))
		for i, t := range sibling.Tools {
			tools[i] = tsrender.MCPToolRender{
				Path: paths[t.Name], Name: t.Name,
				Description: t.Description, InputSchema: t.InputSchema,
			}
		}
		if len(tools) > 0 {
			namespaces = append(namespaces, tsrender.NamespaceRender{
				Namespace: binding.SiblingNamespace(sibling.Slug), Tools: tools,
			})
		}
	}
	return tsrender.RenderNestedRoot("agent", namespaces), nil
}

func toolNames(tools []ToolInfo) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
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
// admin variant (used by tests). The caller value is also stamped
// onto data.CallerAccess so templates can branch on tier
// (e.g. `{{ if .CallerAccess.IsPublic }}…{{ end }}`).
func Render(data AgentData, caller string) (string, error) {
	if caller != "" {
		data = filterAgentData(data, caller)
	}
	data.CallerAccess = Caller(caller)
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
	out.Directories = filterDirs(data.Directories, caller)
	out.ExecEndpoints = filterExecEndpoints(data.ExecEndpoints, caller)
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

// filterDirs keeps a directory entry if the caller can do *anything*
// with it (read OR write OR list satisfied). A caller with no cap on
// the dir shouldn't be told the path exists.
func filterDirs(in []DirInfo, caller string) []DirInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]DirInfo, 0, len(in))
	for _, d := range in {
		if callerSatisfies(caller, d.Read) || callerSatisfies(caller, d.Write) || callerSatisfies(caller, d.List) {
			out = append(out, d)
		}
	}
	return out
}

func filterExecEndpoints(in []ExecEndpointInfo, caller string) []ExecEndpointInfo {
	if len(in) == 0 {
		return in
	}
	out := make([]ExecEndpointInfo, 0, len(in))
	for _, e := range in {
		if callerSatisfies(caller, e.Access) {
			out = append(out, e)
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
