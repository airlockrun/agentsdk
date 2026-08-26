package agentsdk

import (
	"regexp"

	"github.com/airlockrun/goai/tool"
)

// RegisterOption layers agentsdk-only concerns onto a registered tool that
// goai's provider-agnostic tool.Tool doesn't model.
type RegisterOption func(*registeredTool)

// WithLLMHint adds model-only guidance appended to the tool's description in the
// system prompt — kept out of member-facing UIs that render the bare
// description (e.g. the dashboard's Tools tab).
func WithLLMHint(hint string) RegisterOption {
	return func(rt *registeredTool) { rt.llmHint = hint }
}

// RegisterTool registers a goai tool.Tool the LLM can invoke, at the given
// access level. Build the tool once with
// tool.Typed[In,Out] (or tool.New) and pass the same value here and to the
// agent's GenerateText/StreamText sub-calls.
//
//	calc := tool.Typed[CalcIn, CalcOut]("calculator").
//	    Description("Evaluate an expression.").
//	    Execute(doCalc).
//	    Build()
//	agent.RegisterTool(calc, agentsdk.AccessUser)
func (a *Agent) RegisterTool(t tool.Tool, access Access, opts ...RegisterOption) {
	done := a.beginRegistration("RegisterTool")
	defer done()
	rt := &registeredTool{Tool: cloneTool(t), access: access}
	for _, o := range opts {
		if o == nil {
			panic("agentsdk: RegisterTool: nil RegisterOption")
		}
		o(rt)
	}
	validateRegisteredTool(rt)
	if _, exists := a.tools[rt.Name]; exists {
		panic("agentsdk: duplicate RegisterTool: " + rt.Name)
	}
	a.tools[rt.Name] = rt
}

// RegisterWebhook installs a webhook handler at /webhook/{Path}. Synced to
// Airlock on Serve() so external callers can reach it via the agent's
// webhook ingress endpoint.
func (a *Agent) RegisterWebhook(w *Webhook) {
	done := a.beginRegistration("RegisterWebhook")
	defer done()
	if w == nil {
		panic("agentsdk: RegisterWebhook: nil *Webhook")
	}
	copy := *w
	validateWebhook(&copy)
	if _, exists := a.webhooks[copy.Path]; exists {
		panic("agentsdk: duplicate RegisterWebhook: " + copy.Path)
	}
	a.webhooks[copy.Path] = &copy
}

// RegisterRoute installs a custom HTTP route served by this agent and
// proxied via Airlock's subdomain routing.
func (a *Agent) RegisterRoute(r *Route) {
	done := a.beginRegistration("RegisterRoute")
	defer done()
	if r == nil {
		panic("agentsdk: RegisterRoute: nil *Route")
	}
	copy := *r
	validateRoute(&copy)
	key := copy.Method + " " + copy.Path
	if _, exists := a.routes[key]; exists {
		panic("agentsdk: duplicate RegisterRoute: " + key)
	}
	validateRoutePatterns(a.routes, &copy)
	a.routes[key] = &copy
}

// RegisterTopic declares a topic the agent can publish notifications to.
// Synced to Airlock on Serve(). Use the returned *TopicHandle for
// compile-time-bound publishing:
//
//	alerts := agent.RegisterTopic(&agentsdk.Topic{Slug: "alerts", Description: "System alerts"})
//	alerts.Publish(ctx, []DisplayPart{{Type: "text", Text: "Server restarted"}})
func (a *Agent) RegisterTopic(t *Topic) *TopicHandle {
	done := a.beginRegistration("RegisterTopic")
	defer done()
	if t == nil {
		panic("agentsdk: RegisterTopic: nil *Topic")
	}
	copy := *t
	validateTopic(&copy)
	if _, exists := a.topics[copy.Slug]; exists {
		panic("agentsdk: duplicate RegisterTopic: " + copy.Slug)
	}
	a.topics[copy.Slug] = &copy
	return &TopicHandle{slug: copy.Slug, perUser: copy.PerUser, agent: a}
}

// RegisterConnection registers an outgoing service connection and returns a
// handle for proxied requests. Synced to Airlock on Serve(). Use the
// returned handle for compile-time-bound proxy calls:
//
//	gmail := agent.RegisterConnection(&agentsdk.Connection{
//	    Slug: "gmail", Name: "Gmail", BaseURL: "https://gmail.googleapis.com", ...,
//	})
//	body, err := gmail.Request(ctx, agentsdk.RequestOpts{Path: "/messages"})
func (a *Agent) RegisterConnection(c *Connection) *ConnectionHandle {
	done := a.beginRegistration("RegisterConnection")
	defer done()
	if c == nil {
		panic("agentsdk: RegisterConnection: nil *Connection")
	}
	copy := *c
	copy.Scopes = append([]string(nil), c.Scopes...)
	copy.AuthParams = cloneStringMap(c.AuthParams)
	copy.Headers = cloneStringMap(c.Headers)
	validateConnection(&copy)
	if _, exists := a.auths[copy.Slug]; exists {
		panic("agentsdk: duplicate RegisterConnection: " + copy.Slug)
	}
	a.auths[copy.Slug] = &copy
	return &ConnectionHandle{slug: copy.Slug, agent: a}
}

// RegisterEnvVar declares an operator-configured environment variable
// the agent will read at runtime. Returned handle's Get(ctx) fetches the
// value through Airlock; operators populate the value via the agent's
// "Environment" tab in the admin UI.
//
// See the EnvVar type doc for the Secret flag's semantics (write-only UI
// + redaction).
//
//	bbKey := agent.RegisterEnvVar(&agentsdk.EnvVar{
//	    Slug:        "browserbase_api_key",
//	    Description: "Browserbase API key",
//	    Secret:      true,
//	})
//	// later, inside a tool:
//	key, err := bbKey.Get(ctx)
func (a *Agent) RegisterEnvVar(e *EnvVar) *EnvVarHandle {
	done := a.beginRegistration("RegisterEnvVar")
	defer done()
	if e == nil {
		panic("agentsdk: RegisterEnvVar: nil *EnvVar")
	}
	copy := *e
	validateEnvVar(&copy)
	if _, exists := a.envVars[copy.Slug]; exists {
		panic("agentsdk: duplicate RegisterEnvVar: " + copy.Slug)
	}
	var compiled *regexp.Regexp
	if copy.Pattern != "" {
		re, _ := regexp.Compile(copy.Pattern)
		compiled = re
	}
	a.envVars[copy.Slug] = &copy
	return &EnvVarHandle{slug: copy.Slug, secret: copy.Secret, defaultValue: copy.Default, pattern: compiled, agent: a}
}

// RegisterDirectory declares an S3-backed directory at the given path,
// gated by independent Read / Write / List caps. Inside run_js the flat
// verbs (fileRead, fileWrite, fileList, fileDelete, fileStat, fileReadBytes,
// fileExists) check the calling run's access against the directory's
// caps via ResolveFilePath.
//
// Path is S3-style: no leading '/', no trailing '/', e.g. "uploads" or
// "reports/q1". A leading slash is rejected — the LLM and builders share
// one canonical form. Files under the directory are addressed as
// "uploads/doc.pdf", never "/uploads/doc.pdf".
//
// Builder Go code reads and writes the directory through the trusted
// file API (agent.OpenFile / ReadFile / WriteFile / StatFile / ListDir /
// DeleteFile) — these methods do NOT call ResolveFilePath, on the
// principle that builder code that constructs paths itself is trusted.
// When a builder tool accepts a path from the LLM (typed as `string` on
// an Input struct), the builder must call agent.ResolveFilePath
// explicitly before passing the path anywhere.
//
// The framework reserves "tmp" for its own scratch (truncated tool
// output, generated media) at Read=Write=List=AccessUser. Builders may
// call RegisterDirectory("tmp", ...) to override the description; the
// access caps are kept at the framework's defaults.
//
//	agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
//	    Read: agentsdk.AccessUser, Write: agentsdk.AccessUser, List: agentsdk.AccessUser,
//	    Description: "User uploads",
//	})
//	err := agent.WriteFile(ctx, "uploads/doc.pdf", reader, "application/pdf")
func (a *Agent) RegisterDirectory(path string, opts DirectoryOpts) {
	done := a.beginRegistration("RegisterDirectory")
	defer done()
	canon, err := normalizePath(path)
	if err != nil {
		panic("agentsdk: RegisterDirectory: " + err.Error())
	}
	for _, d := range a.directories {
		if d.Path == canon {
			// Reserved framework directory — keep the framework's caps,
			// but allow the builder's description through. Anywhere else
			// duplicate registrations panic so builders find conflicts
			// at startup.
			if canon == reservedTmpPath || canon == reservedIncomingPath || canon == reservedSiblingsPath {
				if opts.Description != "" {
					d.Description = opts.Description
				}
				return
			}
			panic("agentsdk: duplicate RegisterDirectory: " + canon)
		}
	}
	d := &directory{
		Path:           canon,
		Read:           opts.Read,
		Write:          opts.Write,
		List:           opts.List,
		Description:    opts.Description,
		LLMHint:        opts.LLMHint,
		RetentionHours: opts.RetentionHours,
		Scope:          opts.Scope,
	}
	validateDirectory(d)
	a.directories = append(a.directories, d)
}

// RegisterMCP registers a remote MCP server dependency and returns a handle
// for calling its tools. Synced to Airlock on Serve(). Use the returned
// handle for compile-time-bound tool calls:
//
//	github := agent.RegisterMCP(&agentsdk.MCP{Slug: "github", URL: "https://api.github.com/mcp"})
//	result, err := github.CallTool(ctx, "search_repos", args)
func (a *Agent) RegisterMCP(m *MCP) *MCPHandle {
	done := a.beginRegistration("RegisterMCP")
	defer done()
	if m == nil {
		panic("agentsdk: RegisterMCP: nil *MCP")
	}
	copy := *m
	copy.Scopes = append([]string(nil), m.Scopes...)
	validateMCP(&copy)
	if _, exists := a.mcps[copy.Slug]; exists {
		panic("agentsdk: duplicate RegisterMCP: " + copy.Slug)
	}
	a.mcps[copy.Slug] = &copy
	return &MCPHandle{slug: copy.Slug, agent: a}
}
