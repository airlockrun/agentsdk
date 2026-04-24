package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// syncWithAirlock registers connections, MCP servers, webhooks, crons, topics, and event subscriptions with Airlock.
// Called by Serve() before starting the HTTP server. Panics on failure.
func (a *Agent) syncWithAirlock(ctx context.Context) {
	// Register each connection.
	for slug, def := range a.auths {
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/connections/"+slug, def, nil); err != nil {
			panic("agentsdk: failed to register connection " + slug + ": " + err.Error())
		}
	}

	// Register each MCP server.
	for slug, def := range a.mcps {
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/mcp-servers/"+slug, def, nil); err != nil {
			panic("agentsdk: failed to register MCP server " + slug + ": " + err.Error())
		}
	}

	// Build sync payload.
	webhooks := make([]WebhookDef, 0, len(a.webhooks))
	for path, entry := range a.webhooks {
		timeout := entry.opts.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		webhooks = append(webhooks, WebhookDef{
			Path:        path,
			Verify:      entry.opts.Verify,
			Header:      entry.opts.Header,
			TimeoutMs:   timeout.Milliseconds(),
			Description: entry.opts.Description,
		})
	}
	crons := make([]CronEntry, 0, len(a.crons))
	for name, entry := range a.crons {
		timeout := entry.opts.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		crons = append(crons, CronEntry{
			Name:        name,
			Schedule:    entry.schedule,
			TimeoutMs:   timeout.Milliseconds(),
			Description: entry.opts.Description,
		})
	}
	routes := make([]RouteDef, 0, len(a.routes))
	for key, entry := range a.routes {
		// key is "METHOD /path" — split back into method and path.
		method, path, _ := strings.Cut(key, " ")
		routes = append(routes, RouteDef{
			Path:        path,
			Method:      method,
			Access:      string(entry.opts.Access),
			Description: entry.opts.Description,
		})
	}

	topics := make([]TopicDef, 0, len(a.topics))
	for _, def := range a.topics {
		topics = append(topics, def)
	}

	tools := make([]SyncToolDef, 0, len(a.tools))
	for _, t := range a.tools {
		inRaw, err := json.Marshal(t.InputSchema)
		if err != nil {
			panic(fmt.Sprintf("agentsdk: sync: marshal input schema for tool %q: %v", t.Name, err))
		}
		outRaw, err := json.Marshal(t.OutputSchema)
		if err != nil {
			panic(fmt.Sprintf("agentsdk: sync: marshal output schema for tool %q: %v", t.Name, err))
		}
		tools = append(tools, SyncToolDef{
			Name:          t.Name,
			Description:   t.Description,
			Access:        string(t.Access),
			InputSchema:   inRaw,
			OutputSchema:  outRaw,
			InputExamples: t.InputExamples,
		})
	}

	mcpServers := make([]MCPServerSync, 0, len(a.mcps))
	for slug, def := range a.mcps {
		mcpServers = append(mcpServers, MCPServerSync{
			Slug:     slug,
			Name:     def.Name,
			URL:      def.URL,
			AuthMode: def.AuthMode,
			AuthURL:  def.AuthURL,
			TokenURL: def.TokenURL,
			Scopes:   def.Scopes,
		})
	}

	syncBody := SyncRequest{
		Version:      Version,
		Description:  a.description,
		Tools:        tools,
		Webhooks:     webhooks,
		Crons:        crons,
		Routes:       routes,
		Topics:       topics,
		MCPServers:   mcpServers,
		ExtraPrompts: a.extraPrompts,
		ModelSlots:   a.modelSlots,
	}

	var syncResp SyncResponse
	if err := a.client.doJSON(ctx, "PUT", "/api/agent/sync", syncBody, &syncResp); err != nil {
		// 409 Conflict from Airlock means agentsdk-version incompatibility —
		// surface a pointer to the remediation so the operator sees it in
		// docker logs alongside the error persisted in the agent's UI.
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "incompatible") {
			panic("agentsdk: sync rejected by Airlock (" + err.Error() + "); this container is out of date — rebuild the agent from the admin UI")
		}
		panic("agentsdk: failed to sync with Airlock: " + err.Error())
	}

	a.systemPrompt = syncResp.SystemPrompt

	// Log MCP auth issues.
	for _, status := range syncResp.MCPAuthStatus {
		if !status.Authorized {
			log.Printf("MCP server %q: authorization required (%s)", status.Slug, status.AuthURL)
		}
	}
}
