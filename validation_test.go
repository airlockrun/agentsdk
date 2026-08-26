package agentsdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistrationValidation(t *testing.T) {
	noopWebhook := func(context.Context, []byte, *EventWriter) error { return nil }
	noopRoute := func(http.ResponseWriter, *http.Request) error { return nil }

	tests := []struct {
		name string
		want string
		call func(*Agent)
	}{
		{
			name: "invalid tool access",
			want: "invalid Access",
			call: func(a *Agent) { a.RegisterTool(addTool("bad_access", "Bad access."), Access("member")) },
		},
		{
			name: "tool name is snake case",
			want: "lowercase snake_case",
			call: func(a *Agent) { a.RegisterTool(addTool("bad-name", "Bad name."), AccessUser) },
		},
		{
			name: "tool name length",
			want: "1-58 characters",
			call: func(a *Agent) {
				a.RegisterTool(addTool("a"+strings.Repeat("b", maxToolNameLength), "Long name."), AccessUser)
			},
		},
		{
			name: "webhook verification is explicit",
			want: "invalid Verify",
			call: func(a *Agent) {
				a.RegisterWebhook(&Webhook{Path: "events", Handler: noopWebhook, Description: "Events"})
			},
		},
		{
			name: "hmac webhook needs header",
			want: "Header is required",
			call: func(a *Agent) {
				a.RegisterWebhook(&Webhook{Path: "events", Handler: noopWebhook, Verify: WebhookVerificationHMAC, Description: "Events"})
			},
		},
		{
			name: "webhook path is one segment",
			want: "one URL-safe path segment",
			call: func(a *Agent) {
				a.RegisterWebhook(&Webhook{Path: "events/push", Handler: noopWebhook, Verify: WebhookVerificationNone, Description: "Events"})
			},
		},
		{
			name: "webhook timeout is not negative",
			want: "must not be negative",
			call: func(a *Agent) {
				a.RegisterWebhook(&Webhook{Path: "events", Handler: noopWebhook, Verify: WebhookVerificationNone, Timeout: -time.Second, Description: "Events"})
			},
		},
		{
			name: "route method is uppercase",
			want: "uppercase HTTP token",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: "get", Path: "/page", Handler: noopRoute, Access: AccessPublic, Description: "Page"})
			},
		},
		{
			name: "route path is absolute",
			want: "absolute HTTP path",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "page", Handler: noopRoute, Access: AccessPublic, Description: "Page"})
			},
		},
		{
			name: "route path is canonical",
			want: "dot, or dot-dot segments",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/page/../admin", Handler: noopRoute, Access: AccessPublic, Description: "Page"})
			},
		},
		{
			name: "framework route is reserved",
			want: "reserved by the framework",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/__air/assets/custom.js", Handler: noopRoute, Access: AccessPublic, Description: "Asset"})
			},
		},
		{
			name: "static route is reserved",
			want: "reserved by the framework",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/static/custom.js", Handler: noopRoute, Access: AccessPublic, Description: "Asset"})
			},
		},
		{
			name: "job route is reserved",
			want: "reserved by the framework",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodPost, Path: "/job/custom/1", Handler: noopRoute, Access: AccessPublic, Description: "Job shadow"})
			},
		},
		{
			name: "conflicting route patterns",
			want: "invalid or conflicting route",
			call: func(a *Agent) {
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/users/{id}", Handler: noopRoute, Access: AccessUser, Description: "User"})
				a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/users/{name}", Handler: noopRoute, Access: AccessUser, Description: "Named user"})
			},
		},
		{
			name: "connection base URL",
			want: "absolute http(s) URL",
			call: func(a *Agent) {
				a.RegisterConnection(&Connection{Slug: "api", Name: "API", Description: "API", BaseURL: "://bad", AuthMode: ConnectionAuthNone, Access: AccessUser})
			},
		},
		{
			name: "binding slug is snake case",
			want: "lowercase snake_case",
			call: func(a *Agent) {
				a.RegisterConnection(&Connection{Slug: "bad__slug", Name: "API", Description: "API", BaseURL: "https://example.com", AuthMode: ConnectionAuthNone, Access: AccessUser})
			},
		},
		{
			name: "binding slug length",
			want: "1-44 characters",
			call: func(a *Agent) {
				a.RegisterConnection(&Connection{Slug: "a" + strings.Repeat("b", maxBindingSlugLength), Name: "API", Description: "API", BaseURL: "https://example.com", AuthMode: ConnectionAuthNone, Access: AccessUser})
			},
		},
		{
			name: "connection auth mode",
			want: "invalid AuthMode",
			call: func(a *Agent) {
				a.RegisterConnection(&Connection{Slug: "api", Name: "API", Description: "API", BaseURL: "https://example.com", AuthMode: ConnectionAuth("basic"), Access: AccessUser})
			},
		},
		{
			name: "connection reserved OAuth auth param",
			want: "AuthParams key \"redirect_URI\" is reserved",
			call: func(a *Agent) {
				a.RegisterConnection(&Connection{Slug: "api", Name: "API", Description: "API", BaseURL: "https://example.com", AuthMode: ConnectionAuthOAuth, AuthURL: "https://example.com/auth", TokenURL: "https://example.com/token", AuthParams: map[string]string{"redirect_URI": "https://evil.example"}, Access: AccessUser})
			},
		},
		{
			name: "mcp URL",
			want: "absolute http(s) URL",
			call: func(a *Agent) {
				a.RegisterMCP(&MCP{Slug: "docs", Name: "Docs", URL: "file:///tmp/mcp", AuthMode: MCPAuthNone, Access: AccessUser})
			},
		},
		{
			name: "directory access is explicit",
			want: "Access is required",
			call: func(a *Agent) {
				a.RegisterDirectory("reports", DirectoryOpts{Write: AccessUser, List: AccessUser, Description: "Reports"})
			},
		},
		{
			name: "topic description",
			want: "Description is required",
			call: func(a *Agent) {
				a.RegisterTopic(&Topic{Slug: "alerts", Access: AccessUser})
			},
		},
		{
			name: "instruction access",
			want: "invalid Access",
			call: func(a *Agent) {
				a.AddInstruction(&Instruction{Text: "Rule", Access: []Access{"member"}})
			},
		},
		{
			name: "model capability",
			want: "invalid Capability",
			call: func(a *Agent) {
				a.RegisterModel(&ModelSlot{Slug: "summary", Capability: ModelCapability("audio"), Description: "Summary"})
			},
		},
		{
			name: "model slug",
			want: "lowercase snake_case",
			call: func(a *Agent) {
				a.RegisterModel(&ModelSlot{Slug: "Bad Slot", Capability: CapText, Description: "Summary"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testAgent(t)
			expectPanicContains(t, tt.want, func() { tt.call(a) })
		})
	}
}

func TestRegisterModelRejectsDuplicateSlug(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "summary", Capability: CapText, Description: "Summary"})
	expectPanicContains(t, "duplicate RegisterModel", func() {
		a.RegisterModel(&ModelSlot{Slug: "summary", Capability: CapVision, Description: "Visual summary"})
	})
}

func TestRegistrationsAreCopied(t *testing.T) {
	a, _ := testAgent(t)
	toolDecl := addTool("copied_tool", "Copied tool.")
	toolDecl.ProviderOptions = map[string]any{"nested": map[string]any{"mode": "original"}}
	wantSchema := string(toolDecl.InputSchema)
	a.RegisterTool(toolDecl, AccessUser)
	toolDecl.InputSchema[0] = '!'
	toolDecl.ProviderOptions["nested"].(map[string]any)["mode"] = "mutated"

	route := &Route{Method: http.MethodGet, Path: "/page", Handler: func(http.ResponseWriter, *http.Request) error { return nil }, Access: AccessPublic, Description: "Page"}
	a.RegisterRoute(route)
	route.Path = "/mutated"
	route.Description = "Mutated"

	connection := &Connection{
		Slug: "api", Name: "API", Description: "Example API", BaseURL: "https://example.com", AuthMode: ConnectionAuthNone, Access: AccessUser,
		Scopes: []string{"read"}, AuthParams: map[string]string{"prompt": "consent"}, Headers: map[string]string{"Accept": "application/json"},
	}
	a.RegisterConnection(connection)
	connection.Scopes[0] = "write"
	connection.AuthParams["prompt"] = "none"
	connection.Headers["Accept"] = "text/plain"

	instruction := &Instruction{Text: "Rule", Access: []Access{AccessUser}}
	a.AddInstruction(instruction)
	instruction.Text = "Mutated"
	instruction.Access[0] = AccessPublic

	model := &ModelSlot{Slug: "summary", Capability: CapText, Description: "Summary"}
	a.RegisterModel(model)
	model.Capability = CapImage

	gotTool := a.tools["copied_tool"]
	if string(gotTool.InputSchema) != wantSchema || gotTool.ProviderOptions["nested"].(map[string]any)["mode"] != "original" {
		t.Fatalf("registered tool changed after caller mutation: %+v", gotTool)
	}
	if _, ok := a.routes["GET /page"]; !ok || a.routes["GET /page"].Description != "Page" {
		t.Fatalf("registered route changed after caller mutation: %+v", a.routes)
	}
	gotConnection := a.auths["api"]
	if gotConnection.Scopes[0] != "read" || gotConnection.AuthParams["prompt"] != "consent" || gotConnection.Headers["Accept"] != "application/json" {
		t.Fatalf("registered connection changed after caller mutation: %+v", gotConnection)
	}
	if a.instructions[0].Text != "Rule" || a.instructions[0].Access[0] != AccessUser {
		t.Fatalf("registered instruction changed after caller mutation: %+v", a.instructions[0])
	}
	if a.modelSlots[0].Capability != CapText {
		t.Fatalf("registered model changed after caller mutation: %+v", a.modelSlots[0])
	}
}

func TestHandlerFreezesRegistrationsAndServesPublicRoute(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterWebhook(&Webhook{
		Path: "anonymous", Verify: WebhookVerificationNone, Description: "Anonymous events",
		Handler: func(context.Context, []byte, *EventWriter) error { return nil },
	})
	a.RegisterRoute(&Route{
		Method: http.MethodGet, Path: "/", Access: AccessPublic, Description: "Public home",
		Handler: func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})

	handler := a.Handler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("public route status = %d, want %d", w.Code, http.StatusNoContent)
	}

	expectPanicContains(t, "registrations are frozen", func() {
		a.RegisterTool(addTool("late_tool", "Late tool."), AccessUser)
	})
}

func TestSyncValidatesBeforeSendingRequests(t *testing.T) {
	a, mock := testAgent(t)
	a.tools["bad"] = &registeredTool{Tool: addTool("bad", "Bad."), access: Access("member")}

	expectPanicContains(t, "invalid Access", func() {
		_ = a.syncWithAirlock(context.Background())
	})
	if requests := mock.Requests(); len(requests) != 0 {
		t.Fatalf("sync sent %d requests before validation", len(requests))
	}
}

func expectPanicContains(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if got := fmt.Sprint(recovered); !strings.Contains(got, want) {
			t.Fatalf("panic = %q, want substring %q", got, want)
		}
	}()
	fn()
}
