package localruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

type fixtureResources struct {
	mu    sync.Mutex
	calls int
}

func (f *fixtureResources) Connection(_ context.Context, _ wire.Caller, _ string, _ wire.ProxyRequest) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("fixture"))}, nil
}
func (*fixtureResources) MCP(context.Context, wire.Caller, string, wire.MCPToolCallRequest) (wire.MCPToolCallResponse, error) {
	return wire.MCPToolCallResponse{}, errors.New("unexpected MCP fixture call")
}
func (f *fixtureResources) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestHumanResourceOriginCannotBorrowAnotherSessionAfterRestart(t *testing.T) {
	base, state, _, _ := testHost(t, completeModel(t))
	resource := &fixtureResources{}
	config := base.config
	config.Resources = resource
	config.ResourceSession = uuid.NewString()
	first, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.update(t.Context(), func(d *stateData) error {
		d.Manifest.Connections = []wire.ConnectionDef{{Slug: "personal", AuthMode: wire.ConnectionAuthToken, BindingMode: wire.BindingPerUser}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	user := wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}
	caller := wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}
	scope, err := first.Admit(t.Context(), caller)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Airlock-Run-ID": []string{scope.RunID}, wire.InvocationTokenHeader: []string{scope.InvocationToken}}
	server := httptest.NewServer(first.Handler())
	defer server.Close()
	request := wire.ProxyRequest{Method: "GET", Path: "/fixture"}
	if status, body := hostRequest(t, server, "POST", "/api/agent/proxy/personal", request, headers); status != 200 || string(body) != "fixture" {
		t.Fatalf("resource=%d %s", status, body)
	}
	config.ResourceSession = uuid.NewString()
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	second := httptest.NewServer(restarted.Handler())
	defer second.Close()
	if status, _ := hostRequest(t, second, "POST", "/api/agent/proxy/personal", request, headers); status != 403 {
		t.Fatal("persisted human origin borrowed the replacement session")
	}
	if resource.count() != 1 {
		t.Fatalf("rejected session reached resource: %d", resource.count())
	}
	appScope, err := restarted.Admit(t.Context(), appCaller("background"))
	if err != nil {
		t.Fatal(err)
	}
	headers = http.Header{"X-Airlock-Run-ID": []string{appScope.RunID}, wire.InvocationTokenHeader: []string{appScope.InvocationToken}}
	if status, _ := hostRequest(t, second, "POST", "/api/agent/proxy/personal", request, headers); status != 403 {
		t.Fatal("app-owned call borrowed personal binding")
	}
	if resource.count() != 1 {
		t.Fatal("app-owned personal request reached adapter")
	}
}
