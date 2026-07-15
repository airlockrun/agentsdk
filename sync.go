package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
)

// syncWithAirlock registers connections, MCP servers, webhooks, crons, topics, and event subscriptions with Airlock.
// Called by Serve() at startup (via syncOrPanic) and by the /refresh handler.
// Returns the error so /refresh can propagate it; startup panics via the wrapper.
func (a *Agent) syncWithAirlock(ctx context.Context) error {
	a.freeze()
	// Declare each connection as a need in the sync batch. The agent declares
	// the shape; operators create + bind the backing resource.
	connections := make([]wire.ConnectionDef, 0, len(a.auths))
	for slug, c := range a.auths {
		connections = append(connections, wire.ConnectionDef{
			Slug:              slug,
			Name:              c.Name,
			Description:       c.Description,
			BaseURL:           c.BaseURL,
			AuthMode:          wire.ConnectionAuth(c.AuthMode),
			AuthURL:           c.AuthURL,
			TokenURL:          c.TokenURL,
			Scopes:            c.Scopes,
			AuthParams:        c.AuthParams,
			Headers:           c.Headers,
			AuthInjection:     toWireAuthInjection(c.AuthInjection),
			SetupInstructions: c.SetupInstructions,
			LLMHint:           c.LLMHint,
			Access:            toWireAccess(c.Access),
		})
	}

	// Declare each exec endpoint as a need in the sync batch. Operators set
	// transport, host, user, and credentials on the backing resource via the
	// admin UI; we only declare the slug+description+access here.
	execEndpoints := make([]wire.ExecEndpointDef, 0, len(a.execEndpoints))
	for slug, e := range a.execEndpoints {
		execEndpoints = append(execEndpoints, wire.ExecEndpointDef{
			Slug:        slug,
			Description: e.Description,
			LLMHint:     e.LLMHint,
			Access:      toWireAccess(e.Access),
		})
	}

	// Register each env var slot. Operators set values separately via the
	// admin UI; we only declare the slot here.
	for slug, e := range a.envVars {
		def := wire.EnvVarDef{
			Description: e.Description,
			Secret:      e.Secret,
			Default:     e.Default,
			Pattern:     e.Pattern,
		}
		if err := a.client.doJSON(ctx, "PUT", "/api/agent/env-vars/"+slug, def, nil); err != nil {
			return fmt.Errorf("register env var %s: %w", slug, err)
		}
	}

	// Build sync payload — convert builder structs to wire formats.
	webhooks := make([]wire.WebhookDef, 0, len(a.webhooks))
	for _, w := range a.webhooks {
		timeout := w.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		webhooks = append(webhooks, wire.WebhookDef{
			Path:        w.Path,
			Verify:      w.Verify,
			Header:      w.Header,
			TimeoutMs:   timeout.Milliseconds(),
			Description: w.Description,
		})
	}
	scheduleHandlers := make([]wire.ScheduleHandlerDef, 0, len(a.scheduleHandlers))
	for _, h := range a.scheduleHandlers {
		timeout := h.timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		scheduleHandlers = append(scheduleHandlers, wire.ScheduleHandlerDef{
			Slug:        h.slug,
			Kind:        h.kind,
			Recurrence:  h.recurrence,
			TimeoutMs:   timeout.Milliseconds(),
			Description: h.description,
		})
	}
	routeCount := len(a.routes)
	if len(a.staticAssets) > 0 {
		routeCount++
	}
	routes := make([]wire.RouteDef, 0, routeCount)
	for _, r := range a.routes {
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

	topics := make([]wire.TopicDef, 0, len(a.topics))
	for _, t := range a.topics {
		topics = append(topics, wire.TopicDef{
			Slug:        t.Slug,
			Description: t.Description,
			LLMHint:     t.LLMHint,
			Access:      toWireAccess(t.Access),
			PerUser:     t.PerUser,
		})
	}

	tools := make([]wire.ToolDef, 0, len(a.tools))
	for _, t := range a.tools {
		examples := make([]json.RawMessage, len(t.InputExamples))
		for i, ex := range t.InputExamples {
			examples[i] = ex.Input
		}
		tools = append(tools, wire.ToolDef{
			Name:          t.Name,
			Description:   t.Description,
			LLMHint:       t.llmHint,
			Access:        toWireAccess(t.access),
			InputSchema:   t.InputSchema,
			OutputSchema:  t.OutputSchema,
			InputExamples: examples,
		})
	}

	mcpServers := make([]wire.MCPDef, 0, len(a.mcps))
	for _, m := range a.mcps {
		mcpServers = append(mcpServers, wire.MCPDef{
			Slug:          m.Slug,
			Name:          m.Name,
			URL:           m.URL,
			AuthMode:      wire.MCPAuth(m.AuthMode),
			AuthURL:       m.AuthURL,
			TokenURL:      m.TokenURL,
			Scopes:        m.Scopes,
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

	modelSlots := make([]wire.ModelSlotDef, 0, len(a.modelSlots))
	for _, s := range a.modelSlots {
		modelSlots = append(modelSlots, wire.ModelSlotDef{
			Slug:        s.Slug,
			Capability:  string(s.Capability),
			Description: s.Description,
		})
	}

	syncBody := wire.SyncRequest{
		Version:          Version,
		Description:      a.description,
		Emoji:            a.emoji,
		Tools:            tools,
		Webhooks:         webhooks,
		ScheduleHandlers: scheduleHandlers,
		Routes:           routes,
		Topics:           topics,
		MCPServers:       mcpServers,
		Connections:      connections,
		ExecEndpoints:    execEndpoints,
		Directories:      directories,
		Instructions:     instructions,
		ModelSlots:       modelSlots,
	}

	var syncResp wire.SyncResponse
	if err := a.client.doJSON(ctx, "PUT", "/api/agent/sync", syncBody, &syncResp); err != nil {
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

// syncOrPanic is the startup wrapper that turns sync failures into panics —
// preserves the historical "container exits if it can't register" behaviour
// so a misconfigured agent fails loud instead of running in a degraded state.
func (a *Agent) syncOrPanic(ctx context.Context) {
	if err := a.syncWithAirlock(ctx); err != nil {
		panic("agentsdk: " + err.Error())
	}
}
