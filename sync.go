package agentsdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
)

// syncWithAirlock registers the agent's declared capabilities with Airlock.
// Called by Serve() at startup (via syncOrPanic) and by the /refresh handler.
// Returns the error so /refresh can propagate it; startup panics via the wrapper.
func (a *Agent) syncWithAirlock(ctx context.Context) error {
	a.requireRuntime("sync")
	a.freeze()
	manifest := a.buildManifest()
	// Environment declarations retain their dedicated reconciliation endpoint;
	// they are also present in the complete manifest so offline inspection and
	// runtime sync describe the same agent.
	for _, def := range manifest.EnvVars {
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/env-vars/"+def.Slug, def, nil); err != nil {
			return fmt.Errorf("register env var %s: %w", def.Slug, err)
		}
	}

	var syncResp wire.SyncResponse
	if err := a.client.doJSON(ctx, "PUT", "/api/agent/sync", manifest, &syncResp); err != nil {
		// 409 Conflict from Airlock means agentsdk-version incompatibility —
		// surface a pointer to the remediation so the operator sees it in
		// docker logs alongside the error persisted in the agent's UI.
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "incompatible") {
			return fmt.Errorf("sync rejected by Airlock (%w); this container is out of date — rebuild the agent from the admin UI", err)
		}
		return fmt.Errorf("sync with Airlock: %w", err)
	}

	a.applySyncResponse(syncResp)

	// Log MCP auth issues.
	for _, status := range syncResp.MCPAuthStatus {
		if !status.Authorized {
			agentLogger().Warn("MCP server authorization required", zap.String("slug", status.Slug), zap.String("auth_url", status.AuthURL))
		}
	}
	return nil
}

// Manifest freezes registrations and returns the complete canonical agent
// declaration used by both offline inspection and runtime synchronization.
func (a *Agent) Manifest() wire.AgentManifest {
	a.freeze()
	return a.buildManifest()
}

func (a *Agent) buildManifest() wire.AgentManifest {
	connectors := make([]wire.ConnectorNeedDef, 0, len(a.connectors))
	for _, slug := range sortedKeys(a.connectors) {
		need := a.connectors[slug]
		connectors = append(connectors, wire.ConnectorNeedDef{Slug: slug, Description: need.Description, Multiple: need.Multiple, Requirement: cloneConnectorRequirement(need.Requires)})
	}
	connections := make([]wire.ConnectionDef, 0, len(a.auths))
	for _, slug := range sortedKeys(a.auths) {
		c := a.auths[slug]
		connections = append(connections, wire.ConnectionDef{
			Slug: slug, Name: c.Name, Description: c.Description, BaseURL: c.BaseURL,
			AuthMode: wire.ConnectionAuth(c.AuthMode), AuthURL: c.AuthURL, TokenURL: c.TokenURL,
			Scopes: append([]string(nil), c.Scopes...), AuthParams: cloneStringMap(c.AuthParams), Headers: cloneStringMap(c.Headers),
			AuthInjection: toWireAuthInjection(c.AuthInjection), SetupInstructions: c.SetupInstructions,
			LLMHint: c.LLMHint, Access: toWireAccess(c.Access),
		})
	}

	envVars := make([]wire.EnvVarDef, 0, len(a.envVars))
	for _, slug := range sortedKeys(a.envVars) {
		e := a.envVars[slug]
		envVars = append(envVars, wire.EnvVarDef{Slug: slug, Description: e.Description, Secret: e.Secret, Default: e.Default, Pattern: e.Pattern})
	}

	webhooks := make([]wire.WebhookDef, 0, len(a.webhooks))
	for _, path := range sortedKeys(a.webhooks) {
		w := a.webhooks[path]
		timeout := w.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		webhooks = append(webhooks, wire.WebhookDef{
			Path:        w.Path,
			Verify:      string(w.Verify),
			Header:      w.Header,
			TimeoutMs:   timeout.Milliseconds(),
			Description: w.Description,
		})
	}
	jobManifest := a.buildJobManifest()
	routeCount := len(a.routes)
	if len(a.staticAssets) > 0 {
		routeCount++
	}
	routes := make([]wire.RouteDef, 0, routeCount)
	for _, key := range sortedKeys(a.routes) {
		r := a.routes[key]
		routes = append(routes, wire.RouteDef{
			Path:        r.Path,
			Method:      r.Method,
			Access:      toWireAccess(r.Access),
			Description: r.Description,
		})
	}
	if len(a.staticAssets) > 0 {
		routes = append(routes, wire.RouteDef{
			Path:        "/static/{name}",
			Method:      http.MethodGet,
			Access:      wire.AccessPublic,
			Description: "Immutable static assets registered by the agent",
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Method == routes[j].Method {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	topics := make([]wire.TopicDef, 0, len(a.topics))
	for _, slug := range sortedKeys(a.topics) {
		t := a.topics[slug]
		topics = append(topics, wire.TopicDef{
			Slug:        t.Slug,
			Description: t.Description,
			LLMHint:     t.LLMHint,
			Access:      toWireAccess(t.Access),
			PerUser:     t.PerUser,
		})
	}

	tools := make([]wire.ToolDef, 0, len(a.tools))
	for _, name := range sortedKeys(a.tools) {
		t := a.tools[name]
		examples := make([]json.RawMessage, len(t.InputExamples))
		for i, ex := range t.InputExamples {
			examples[i] = append(json.RawMessage(nil), ex.Input...)
		}
		tools = append(tools, wire.ToolDef{
			Name:          t.Name,
			Description:   t.Description,
			LLMHint:       t.llmHint,
			Access:        toWireAccess(t.access),
			InputSchema:   append(json.RawMessage(nil), t.InputSchema...),
			OutputSchema:  append(json.RawMessage(nil), t.OutputSchema...),
			InputExamples: examples,
		})
	}

	mcpServers := make([]wire.MCPDef, 0, len(a.mcps))
	for _, slug := range sortedKeys(a.mcps) {
		m := a.mcps[slug]
		mcpServers = append(mcpServers, wire.MCPDef{
			Slug:          m.Slug,
			Name:          m.Name,
			URL:           m.URL,
			AuthMode:      wire.MCPAuth(m.AuthMode),
			AuthURL:       m.AuthURL,
			TokenURL:      m.TokenURL,
			Scopes:        append([]string(nil), m.Scopes...),
			AuthInjection: toWireAuthInjection(m.AuthInjection),
			Access:        toWireAccess(m.Access),
		})
	}

	instructions := make([]wire.InstructionDef, 0, len(a.instructions))
	for _, ep := range a.instructions {
		instructions = append(instructions, wire.InstructionDef{
			Text:   ep.Text,
			Access: toWireAccesses(ep.Access),
		})
	}

	directories := make([]wire.DirectoryDef, 0, len(a.directories))
	for _, d := range a.directories {
		directories = append(directories, wire.DirectoryDef{
			Path:           d.Path,
			Read:           toWireAccess(d.Read),
			Write:          toWireAccess(d.Write),
			List:           toWireAccess(d.List),
			Description:    d.Description,
			LLMHint:        d.LLMHint,
			RetentionHours: d.RetentionHours,
			Scope:          wire.DirectoryScope(d.Scope),
		})
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })

	modelSlots := make([]wire.ModelSlotDef, 0, len(a.modelSlots))
	for _, s := range a.modelSlots {
		modelSlots = append(modelSlots, wire.ModelSlotDef{
			Slug:        s.Slug,
			Capability:  string(s.Capability),
			Description: s.Description,
		})
	}
	sort.Slice(modelSlots, func(i, j int) bool { return modelSlots[i].Slug < modelSlots[j].Slug })

	staticAssets := make([]wire.StaticAssetDef, 0, len(a.staticAssets))
	for _, name := range sortedKeys(a.staticAssets) {
		asset := a.staticAssets[name]
		digest := sha256.Sum256(asset.Data)
		staticAssets = append(staticAssets, wire.StaticAssetDef{
			Name: name, ContentType: asset.ContentType, Size: int64(len(asset.Data)), SHA256: fmt.Sprintf("%x", digest),
		})
	}
	startupHooks := make([]wire.StartupHookDef, 0, len(a.startHooks))
	for _, hook := range a.startHooks {
		startupHooks = append(startupHooks, wire.StartupHookDef{Name: hook.name})
	}

	return wire.AgentManifest{
		Version:      Version,
		Description:  a.description,
		Emoji:        a.emoji,
		Tools:        tools,
		Webhooks:     webhooks,
		JobHandlers:  jobManifest.JobHandlers,
		JobCrons:     jobManifest.JobCrons,
		Routes:       routes,
		Topics:       topics,
		MCPServers:   mcpServers,
		Connections:  connections,
		EnvVars:      envVars,
		Directories:  directories,
		Instructions: instructions,
		ModelSlots:   modelSlots,
		StaticAssets: staticAssets,
		StartupHooks: startupHooks,
		Connectors:   connectors,
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (a *Agent) buildJobManifest() wire.JobManifest {
	jobKeys := make([]jobKey, 0, len(a.jobs))
	for key := range a.jobs {
		jobKeys = append(jobKeys, key)
	}
	sort.Slice(jobKeys, func(i, j int) bool {
		if jobKeys[i].name == jobKeys[j].name {
			return jobKeys[i].version < jobKeys[j].version
		}
		return jobKeys[i].name < jobKeys[j].name
	})
	jobHandlers := make([]wire.JobHandlerDef, 0, len(jobKeys))
	for _, key := range jobKeys {
		job := a.jobs[key]
		jobHandlers = append(jobHandlers, wire.JobHandlerDef{
			Name:             job.name,
			Version:          int32(job.version),
			Description:      job.description,
			TimeoutMs:        job.timeout.Milliseconds(),
			MaxAttempts:      int32(job.maxAttempts),
			MaxConcurrency:   int32(job.maxConcurrency),
			InputSchema:      append(json.RawMessage(nil), job.inputSchema...),
			OutputSchema:     append(json.RawMessage(nil), job.outputSchema...),
			InputSchemaHash:  job.inputSchemaHash,
			OutputSchemaHash: job.outputSchemaHash,
		})
	}
	cronSlugs := make([]string, 0, len(a.jobCrons))
	for slug := range a.jobCrons {
		cronSlugs = append(cronSlugs, slug)
	}
	sort.Strings(cronSlugs)
	jobCrons := make([]wire.JobCronDef, 0, len(cronSlugs))
	for _, slug := range cronSlugs {
		cron := a.jobCrons[slug]
		jobCrons = append(jobCrons, wire.JobCronDef{
			Slug:             cron.slug,
			Schedule:         cron.schedule,
			Description:      cron.description,
			HandlerName:      cron.handlerName,
			HandlerVersion:   int32(cron.handlerVersion),
			InputSchemaHash:  cron.inputSchemaHash,
			OutputSchemaHash: cron.outputSchemaHash,
			Input:            append(json.RawMessage(nil), cron.input...),
		})
	}
	return wire.JobManifest{JobHandlers: jobHandlers, JobCrons: jobCrons}
}
