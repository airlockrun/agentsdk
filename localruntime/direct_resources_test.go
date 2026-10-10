package localruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDirectNoAuthResourcesNeedNoRemoteIdentity(t *testing.T) {
	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if r.URL.Path != "/api/fixture" || r.Header.Get("X-Fixture") != "declared" {
			t.Errorf("request=%s headers=%v", r.URL.Path, r.Header)
		}
		w.Write([]byte("local HTTP"))
	}))
	defer upstream.Close()
	mcpServer := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(mcpServer, &sdk.Tool{Name: "lookup", Description: "Fixture lookup"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, map[string]string, error) {
		return nil, map[string]string{"value": "local MCP"}, nil
	})
	mcpHTTP := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpServer }, &sdk.StreamableHTTPOptions{Stateless: true}))
	defer mcpHTTP.Close()
	manifest := wire.AgentManifest{Connections: []wire.ConnectionDef{{Slug: "public_api", AuthMode: wire.ConnectionAuthNone, BaseURL: upstream.URL + "/api", Headers: map[string]string{"X-Fixture": "declared"}}}, MCPServers: []wire.MCPDef{{Slug: "public_mcp", AuthMode: wire.MCPAuthNone, URL: mcpHTTP.URL}}}
	direct := NewDirectResources(manifest, http.DefaultClient)
	schemas, err := direct.Discover(t.Context(), "public_mcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 1 || schemas[0].Name != "lookup" {
		t.Fatalf("schemas=%+v", schemas)
	}
	out, err := direct.MCP(t.Context(), appCaller("background"), "public_mcp", wire.MCPToolCallRequest{Tool: "lookup", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.StructuredContent) != `{"value":"local MCP"}` {
		t.Fatalf("MCP=%+v", out)
	}
	h, state, _, _ := testHost(t, completeModel(t))
	config := h.config
	config.NoAuthResources = direct
	host, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.update(t.Context(), func(d *stateData) error { d.Manifest.Connections = manifest.Connections; return nil }); err != nil {
		t.Fatal(err)
	}
	user := wire.CallerUser{ID: "fixture-user", PlatformMember: true}
	scope, err := host.Admit(t.Context(), wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host.Handler())
	defer server.Close()
	status, body := hostRequest(t, server, "POST", "/api/agent/proxy/public_api", wire.ProxyRequest{Method: "GET", Path: "/fixture"}, http.Header{"X-Airlock-Run-ID": []string{scope.RunID}, wire.InvocationTokenHeader: []string{scope.InvocationToken}})
	if status != 200 || string(body) != "local HTTP" {
		t.Fatalf("HTTP %d %s", status, body)
	}
	for _, path := range []string{"https://other.example/path", "//other.example/path"} {
		if resp, err := direct.Connection(t.Context(), appCaller("background"), "public_api", wire.ProxyRequest{Path: path}); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			t.Fatal("caller changed declared origin")
		}
	}
	if httpCalls != 1 {
		t.Fatalf("unexpected HTTP calls=%d", httpCalls)
	}
}
