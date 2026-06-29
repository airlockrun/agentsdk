package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// syncWithAirlock registers connections, MCP servers, webhooks, crons, topics, and event subscriptions with Airlock.
// Called by Serve() at startup (via syncOrPanic) and by the /refresh handler.
// Returns the error so /refresh can propagate it; startup panics via the wrapper.
func (a *Agent) syncWithAirlock(ctx context.Context) error {
	// Declare each connection as a need in the sync batch. The agent declares
	// the shape; operators create + bind the backing resource.
	connections := make([]ConnectionDef, 0, len(a.auths))
	for slug, c := range a.auths {
		connections = append(connections, ConnectionDef{
			Slug:              slug,
			Name:              c.Name,
			Description:       c.Description,
			BaseURL:           c.BaseURL,
			AuthMode:          c.AuthMode,
			AuthURL:           c.AuthURL,
			TokenURL:          c.TokenURL,
			Scopes:            c.Scopes,
			AuthParams:        c.AuthParams,
			Headers:           c.Headers,
			AuthInjection:     c.AuthInjection,
			SetupInstructions: c.SetupInstructions,
			LLMHint:           c.LLMHint,
			Access:            c.Access,
		})
	}

	// Declare each exec endpoint as a need in the sync batch. Operators set
	// transport, host, user, and credentials on the backing resource via the
	// admin UI; we only declare the slug+description+access here.
	execEndpoints := make([]ExecEndpointDef, 0, len(a.execEndpoints))
	for slug, e := range a.execEndpoints {
		execEndpoints = append(execEndpoints, ExecEndpointDef{
			Slug:        slug,
			Description: e.Description,
			LLMHint:     e.LLMHint,
			Access:      e.Access,
		})
	}

	// Register each env var slot. Operators set values separately via the
	// admin UI; we only declare the slot here.
	for slug, e := range a.envVars {
		def := EnvVarDef{
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
	scheduleHandlers := make([]ScheduleHandlerDef, 0, len(a.scheduleHandlers))
	for _, h := range a.scheduleHandlers {
		timeout := h.timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		scheduleHandlers = append(scheduleHandlers, ScheduleHandlerDef{
			Slug:        h.slug,
			Kind:        h.kind,
			Recurrence:  h.recurrence,
			TimeoutMs:   timeout.Milliseconds(),
			Description: h.description,
		})
	}
	routes := make([]RouteDef, 0, len(a.routes))
	for _, r := range a.routes {
		routes = append(routes, RouteDef{
			Path:        r.Path,
			Method:      r.Method,
			Access:      r.Access,
			Description: r.Description,
		})
	}

	topics := make([]TopicDef, 0, len(a.topics))
	for _, t := range a.topics {
		topics = append(topics, TopicDef{
			Slug:        t.Slug,
			Description: t.Description,
			LLMHint:     t.LLMHint,
			Access:      t.Access,
			PerUser:     t.PerUser,
		})
	}

	tools := make([]ToolDef, 0, len(a.tools))
	for _, t := range a.tools {
		examples := make([]json.RawMessage, len(t.InputExamples))
		for i, ex := range t.InputExamples {
			examples[i] = ex.Input
		}
		tools = append(tools, ToolDef{
			Name:          t.Name,
			Description:   t.Description,
			LLMHint:       t.llmHint,
			Access:        t.access,
			InputSchema:   t.InputSchema,
			OutputSchema:  t.OutputSchema,
			InputExamples: examples,
		})
	}

	mcpServers := make([]MCPDef, 0, len(a.mcps))
	for _, m := range a.mcps {
		mcpServers = append(mcpServers, MCPDef{
			Slug:          m.Slug,
			Name:          m.Name,
			URL:           m.URL,
			AuthMode:      m.AuthMode,
			AuthURL:       m.AuthURL,
			TokenURL:      m.TokenURL,
			Scopes:        m.Scopes,
			AuthInjection: m.AuthInjection,
			Access:        m.Access,
		})
	}

	instructions := make([]InstructionDef, 0, len(a.instructions))
	for _, ep := range a.instructions {
		instructions = append(instructions, InstructionDef{
			Text:   ep.Text,
			Access: ep.Access,
		})
	}

	directories := make([]DirectoryDef, 0, len(a.directories))
	for _, d := range a.directories {
		directories = append(directories, DirectoryDef{
			Path:           d.Path,
			Read:           d.Read,
			Write:          d.Write,
			List:           d.List,
			Description:    d.Description,
			LLMHint:        d.LLMHint,
			RetentionHours: d.RetentionHours,
			Scope:          d.Scope,
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
