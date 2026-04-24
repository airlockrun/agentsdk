package agentsdk

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

// RegisterWebhook registers a webhook handler at /webhook/{path}.
// On Serve(), the agent syncs registered webhooks with Airlock.
func (a *Agent) RegisterWebhook(path string, fn WebhookHandlerFunc, opts WebhookOpts) {
	if _, exists := a.webhooks[path]; exists {
		panic("agentsdk: duplicate RegisterWebhook: " + path)
	}
	if opts.Verify == "" {
		opts.Verify = "none"
	}
	a.webhooks[path] = webhookEntry{handler: fn, opts: opts}
}

// RegisterCron registers a cron job handler.
// Schedule is a standard cron expression (e.g. "0 9 * * *").
// On Serve(), the agent syncs registered crons with Airlock.
func (a *Agent) RegisterCron(name, schedule string, fn CronHandlerFunc, opts CronOpts) {
	if _, exists := a.crons[name]; exists {
		panic("agentsdk: duplicate RegisterCron: " + name)
	}
	a.crons[name] = cronEntry{
		schedule: schedule,
		handler:  fn,
		opts:     opts,
	}
}

// RegisterRoute registers a custom HTTP route served by this agent. The
// route is synced to Airlock and proxied via subdomain routing.
func (a *Agent) RegisterRoute(path, method string, handler RouteHandlerFunc, opts RouteOpts) {
	key := method + " " + path
	if _, exists := a.routes[key]; exists {
		panic("agentsdk: duplicate RegisterRoute: " + key)
	}
	a.routes[key] = routeEntry{handler: handler, opts: opts}
}

// RegisterTopic declares a topic that this agent can publish notifications to.
// On Serve(), the agent syncs registered topics with Airlock.
// Use the returned TopicHandle for compile-time-bound publishing:
//
//	alerts := agent.RegisterTopic("alerts", "System alerts")
//	alerts.Publish(ctx, run, []DisplayPart{{Type: "text", Text: "Server restarted"}})
func (a *Agent) RegisterTopic(slug, description string) *TopicHandle {
	if _, exists := a.topics[slug]; exists {
		panic("agentsdk: duplicate RegisterTopic: " + slug)
	}
	a.topics[slug] = TopicDef{
		Slug:        slug,
		Description: description,
	}
	return &TopicHandle{slug: slug, agent: a}
}

// RegisterConnection registers an outgoing service connection and returns a handle for proxied requests.
// On Serve(), the agent registers this with Airlock via PUT /api/agent/connections/{slug}.
// Use the returned ConnectionHandle for compile-time-bound proxy calls:
//
//	gmail := agent.RegisterConnection("gmail", ConnectionDef{...})
//	body, err := gmail.Request(ctx, "GET", "/messages", nil)
func (a *Agent) RegisterConnection(slug string, def ConnectionDef) *ConnectionHandle {
	if _, exists := a.auths[slug]; exists {
		panic("agentsdk: duplicate RegisterConnection: " + slug)
	}
	a.auths[slug] = def
	return &ConnectionHandle{slug: slug, agent: a}
}

// RegisterMCP registers a remote MCP server dependency and returns a handle for calling its tools.
// On Serve(), the agent registers this with Airlock via PUT /api/agent/mcp-servers/{slug}.
// Use the returned MCPHandle for compile-time-bound tool calls:
//
//	github := agent.RegisterMCP("github", MCPDef{URL: "https://api.github.com/mcp"})
//	result, err := github.CallTool(ctx, run, "search_repos", args)
func (a *Agent) RegisterMCP(slug string, def MCPDef) *MCPHandle {
	if _, exists := a.mcps[slug]; exists {
		panic("agentsdk: duplicate RegisterMCP: " + slug)
	}
	if def.URL == "" {
		panic("agentsdk: RegisterMCP(" + slug + "): URL is required")
	}
	a.mcps[slug] = def
	return &MCPHandle{slug: slug, agent: a}
}

