package agentsdk

import "fmt"

// RegisterTool registers a typed, schema-bearing capability the LLM can
// invoke via run_js. The tool auto-binds as a global inside the goja VM;
// input from JS is JSON-marshaled and decoded into the author's In struct,
// Execute runs, and the Out struct is JSON-marshaled back to a native JS
// value. Input/output JSON schemas are rendered as a TypeScript declaration
// in the system prompt so the LLM sees typed signatures.
//
//	agent.RegisterTool(&agentsdk.Tool[SearchIn, SearchOut]{
//	    Name:        "search",
//	    Description: "Search the web.",
//	    Execute:     doSearch,
//	    Access:      agentsdk.AccessUser,
//	})
func (a *Agent) RegisterTool(def ToolDef) {
	rt := def.toRegistered()
	if _, exists := a.tools[rt.Name]; exists {
		panic("agentsdk: duplicate RegisterTool: " + rt.Name)
	}
	a.tools[rt.Name] = rt
}

// RegisterWebhook installs a webhook handler at /webhook/{Path}. Synced to
// Airlock on Serve() so external callers can reach it via the agent's
// webhook ingress endpoint.
func (a *Agent) RegisterWebhook(w *Webhook) {
	if w == nil {
		panic("agentsdk: RegisterWebhook: nil *Webhook")
	}
	if w.Path == "" {
		panic("agentsdk: RegisterWebhook: Path is required")
	}
	if w.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): Handler is required", w.Path))
	}
	if _, exists := a.webhooks[w.Path]; exists {
		panic("agentsdk: duplicate RegisterWebhook: " + w.Path)
	}
	if w.Verify == "" {
		w.Verify = "none"
	}
	if w.Access == "" {
		w.Access = AccessUser
	}
	a.webhooks[w.Path] = w
}

// RegisterCron installs a cron job. Schedule is a standard cron expression
// (e.g. "0 9 * * *"). Synced to Airlock on Serve() so the scheduler can fire it.
func (a *Agent) RegisterCron(c *Cron) {
	if c == nil {
		panic("agentsdk: RegisterCron: nil *Cron")
	}
	if c.Name == "" {
		panic("agentsdk: RegisterCron: Name is required")
	}
	if c.Schedule == "" {
		panic(fmt.Sprintf("agentsdk: RegisterCron(%q): Schedule is required", c.Name))
	}
	if c.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterCron(%q): Handler is required", c.Name))
	}
	if _, exists := a.crons[c.Name]; exists {
		panic("agentsdk: duplicate RegisterCron: " + c.Name)
	}
	a.crons[c.Name] = c
}

// RegisterRoute installs a custom HTTP route served by this agent and
// proxied via Airlock's subdomain routing.
func (a *Agent) RegisterRoute(r *Route) {
	if r == nil {
		panic("agentsdk: RegisterRoute: nil *Route")
	}
	if r.Method == "" {
		panic("agentsdk: RegisterRoute: Method is required")
	}
	if r.Path == "" {
		panic("agentsdk: RegisterRoute: Path is required")
	}
	if r.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %s): Handler is required", r.Method, r.Path))
	}
	if r.Access == "" {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %s): Access is required", r.Method, r.Path))
	}
	key := r.Method + " " + r.Path
	if _, exists := a.routes[key]; exists {
		panic("agentsdk: duplicate RegisterRoute: " + key)
	}
	a.routes[key] = r
}

// RegisterTopic declares a topic the agent can publish notifications to.
// Synced to Airlock on Serve(). Use the returned *TopicHandle for
// compile-time-bound publishing:
//
//	alerts := agent.RegisterTopic(&agentsdk.Topic{Slug: "alerts", Description: "System alerts"})
//	alerts.Publish(ctx, []DisplayPart{{Type: "text", Text: "Server restarted"}})
func (a *Agent) RegisterTopic(t *Topic) *TopicHandle {
	if t == nil {
		panic("agentsdk: RegisterTopic: nil *Topic")
	}
	if t.Slug == "" {
		panic("agentsdk: RegisterTopic: Slug is required")
	}
	if _, exists := a.topics[t.Slug]; exists {
		panic("agentsdk: duplicate RegisterTopic: " + t.Slug)
	}
	if t.Access == "" {
		t.Access = AccessUser
	}
	a.topics[t.Slug] = t
	return &TopicHandle{slug: t.Slug, agent: a}
}

// RegisterConnection registers an outgoing service connection and returns a
// handle for proxied requests. Synced to Airlock on Serve(). Use the
// returned handle for compile-time-bound proxy calls:
//
//	gmail := agent.RegisterConnection(&agentsdk.Connection{
//	    Slug: "gmail", Name: "Gmail", BaseURL: "https://gmail.googleapis.com", ...,
//	})
//	body, err := gmail.Request(ctx, "GET", "/messages", nil)
func (a *Agent) RegisterConnection(c *Connection) *ConnectionHandle {
	if c == nil {
		panic("agentsdk: RegisterConnection: nil *Connection")
	}
	if c.Slug == "" {
		panic("agentsdk: RegisterConnection: Slug is required")
	}
	if _, exists := a.auths[c.Slug]; exists {
		panic("agentsdk: duplicate RegisterConnection: " + c.Slug)
	}
	if c.Access == "" {
		c.Access = AccessUser
	}
	a.auths[c.Slug] = c
	return &ConnectionHandle{slug: c.Slug, agent: a}
}

// RegisterStorage declares an S3-backed storage zone scoped to an Access
// level. Returns a *StorageHandle for builder Go code and exposes a
// `storage_{slug}` JS object inside run_js (only to callers whose access
// satisfies the zone's Access; AccessInternal zones are never exposed
// to JS at all). Slug also becomes the S3 prefix.
//
// The framework reserves the slug "tmp" for its own scratch storage
// (truncated tool output, generated media). Builders may pass
// Slug:"tmp" — the call returns a working handle to the framework's
// tmp zone but the supplied Access / Description are silently ignored.
//
//	uploads := agent.RegisterStorage(&agentsdk.Storage{
//	    Slug: "uploads", Access: agentsdk.AccessUser, Description: "User uploads",
//	})
//	err := uploads.Put(ctx, "doc.pdf", reader, "application/pdf")
func (a *Agent) RegisterStorage(s *Storage) *StorageHandle {
	if s == nil {
		panic("agentsdk: RegisterStorage: nil *Storage")
	}
	if s.Slug == "" {
		panic("agentsdk: RegisterStorage: Slug is required")
	}
	if existing, exists := a.storages[s.Slug]; exists {
		// Reserved framework zone — return the existing handle without
		// overwriting access / description. This lets builder code freely
		// reference `agent.RegisterStorage(&Storage{Slug: "tmp"})`
		// without colliding with the framework's auto-registration.
		if s.Slug == reservedTmpSlug {
			return &StorageHandle{slug: existing.Slug, access: existing.Access, agent: a}
		}
		panic("agentsdk: duplicate RegisterStorage: " + s.Slug)
	}
	if s.Access == "" {
		s.Access = AccessUser
	}
	a.storages[s.Slug] = s
	return &StorageHandle{slug: s.Slug, access: s.Access, agent: a}
}

// RegisterMCP registers a remote MCP server dependency and returns a handle
// for calling its tools. Synced to Airlock on Serve(). Use the returned
// handle for compile-time-bound tool calls:
//
//	github := agent.RegisterMCP(&agentsdk.MCP{Slug: "github", URL: "https://api.github.com/mcp"})
//	result, err := github.CallTool(ctx, "search_repos", args)
func (a *Agent) RegisterMCP(m *MCP) *MCPHandle {
	if m == nil {
		panic("agentsdk: RegisterMCP: nil *MCP")
	}
	if m.Slug == "" {
		panic("agentsdk: RegisterMCP: Slug is required")
	}
	if m.URL == "" {
		panic("agentsdk: RegisterMCP(" + m.Slug + "): URL is required")
	}
	if _, exists := a.mcps[m.Slug]; exists {
		panic("agentsdk: duplicate RegisterMCP: " + m.Slug)
	}
	if m.Access == "" {
		m.Access = AccessUser
	}
	a.mcps[m.Slug] = m
	return &MCPHandle{slug: m.Slug, agent: a}
}
