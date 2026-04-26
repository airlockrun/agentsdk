package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// syncWithAirlock registers connections, MCP servers, webhooks, crons, topics, and event subscriptions with Airlock.
// Called by Serve() at startup (via syncOrPanic) and by the /refresh handler.
// Returns the error so /refresh can propagate it; startup panics via the wrapper.
func (a *Agent) syncWithAirlock(ctx context.Context) error {
	// Register each connection.
	for slug, def := range a.auths {
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/connections/"+slug, def, nil); err != nil {
			return fmt.Errorf("register connection %s: %w", slug, err)
		}
	}

	// Register each MCP server.
	for slug, def := range a.mcps {
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/mcp-servers/"+slug, def, nil); err != nil {
			return fmt.Errorf("register MCP server %s: %w", slug, err)
		}
	}

	// Build sync payload — convert builder structs to wire formats.
	webhooks := make([]WebhookDef, 0, len(a.webhooks))
	for _, w := range a.webhooks {
		timeout := w.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		webhooks = append(webhooks, WebhookDef{
			Path:        w.Path,
			Verify:      w.Verify,
			Header:      w.Header,
			TimeoutMs:   timeout.Milliseconds(),
			Description: w.Description,
		})
	}
	crons := make([]CronEntry, 0, len(a.crons))
	for _, c := range a.crons {
		timeout := c.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		crons = append(crons, CronEntry{
			Name:        c.Name,
			Schedule:    c.Schedule,
			TimeoutMs:   timeout.Milliseconds(),
			Description: c.Description,
		})
	}
	routes := make([]RouteDef, 0, len(a.routes))
	for _, r := range a.routes {
		routes = append(routes, RouteDef{
			Path:        r.Path,
			Method:      r.Method,
			Access:      string(r.Access),
			Description: r.Description,
		})
	}

	topics := make([]TopicDef, 0, len(a.topics))
	for _, t := range a.topics {
		topics = append(topics, TopicDef{
			Slug:        t.Slug,
			Description: t.Description,
			Access:      string(t.Access),
		})
	}

	tools := make([]SyncToolDef, 0, len(a.tools))
	for _, t := range a.tools {
		inRaw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return fmt.Errorf("marshal input schema for tool %q: %w", t.Name, err)
		}
		outRaw, err := json.Marshal(t.OutputSchema)
		if err != nil {
			return fmt.Errorf("marshal output schema for tool %q: %w", t.Name, err)
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
	for _, m := range a.mcps {
		mcpServers = append(mcpServers, MCPServerSync{
			Slug:     m.Slug,
			Name:     m.Name,
			URL:      m.URL,
			AuthMode: m.AuthMode,
			AuthURL:  m.AuthURL,
			TokenURL: m.TokenURL,
			Scopes:   m.Scopes,
			Access:   string(m.Access),
		})
	}

	extraPrompts := make([]ExtraPromptSpec, 0, len(a.extraPrompts))
	for _, ep := range a.extraPrompts {
		extraPrompts = append(extraPrompts, ExtraPromptSpec{
			Text:   ep.Text,
			Access: ep.Access,
		})
	}

	modelSlots := make([]ModelSlotDef, 0, len(a.modelSlots))
	for _, s := range a.modelSlots {
		modelSlots = append(modelSlots, ModelSlotDef{
			Slug:        s.Slug,
			Capability:  string(s.Capability),
			Description: s.Description,
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
		ExtraPrompts: extraPrompts,
		ModelSlots:   modelSlots,
	}

	var syncResp SyncResponse
	if err := a.client.doJSON(ctx, "PUT", "/api/agent/sync", syncBody, &syncResp); err != nil {
		// 409 Conflict from Airlock means agentsdk-version incompatibility —
		// surface a pointer to the remediation so the operator sees it in
		// docker logs alongside the error persisted in the agent's UI.
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "incompatible") {
			return fmt.Errorf("sync rejected by Airlock (%w); this container is out of date — rebuild the agent from the admin UI", err)
		}
		return fmt.Errorf("sync with Airlock: %w", err)
	}

	a.applySyncResponse(syncResp.SystemPrompt, syncResp.MCPSchemas)

	// Log MCP auth issues.
	for _, status := range syncResp.MCPAuthStatus {
		if !status.Authorized {
			log.Printf("MCP server %q: authorization required (%s)", status.Slug, status.AuthURL)
		}
	}
	return nil
}

// syncOrPanic is the startup wrapper that turns sync failures into panics —
// preserves the historical "container exits if it can't register" behaviour
// so a misconfigured agent fails loud instead of running in a degraded state.
func (a *Agent) syncOrPanic(ctx context.Context) {
	if err := a.syncWithAirlock(ctx); err != nil {
		panic("agentsdk: " + err.Error())
	}
}
