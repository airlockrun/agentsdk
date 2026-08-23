package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/internal/mockairlock"
	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
)

func TestHealthEndpoint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(greetTool("test_tool", "Test tool.",
		func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil }), AccessUser)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	a.handleHealth(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status string   `json:"status"`
		Tools  []string `json:"tools"`
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, exists := raw["schedules"]; exists {
		t.Fatal("health response exposes schedules")
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected ok, got %s", resp.Status)
	}
	if len(resp.Tools) != 1 || resp.Tools[0] != "test_tool" {
		t.Fatalf("expected [test_tool], got %v", resp.Tools)
	}
}

// TestHealthEndpointDBUnavailable verifies that when the agent has a DB
// configured but it can't be reached/authenticated, /health reports 503 —
// so the dispatcher keeps the agent out of rotation instead of routing
// traffic that would 500 on the first query (drifted DB role, DB down).
func TestHealthEndpointDBUnavailable(t *testing.T) {
	a, _ := testAgent(t)
	db, err := sql.Open("postgres", "postgres://nope:nope@127.0.0.1:65500/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a.db = &AgentDB{db: db, agent: a}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health", nil)
	a.handleHealth(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when DB unreachable, got %d", w.Code)
	}
	var resp struct {
		Status string `json:"status"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "db_unavailable" {
		t.Fatalf("expected db_unavailable, got %s", resp.Status)
	}
}

func TestRouteContextPropagatesCallerAndAgent(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantAccess Access
	}{
		{name: "anonymous", wantAccess: AccessPublic},
		{name: "known caller", header: string(AccessUser), wantAccess: AccessUser},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			var gotAgent *Agent
			var gotCaller caller
			var gotRunAccess Access
			a.RegisterRoute(&Route{
				Method: http.MethodGet,
				Path:   "/context",
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					gotAgent = AgentFromContext(r.Context())
					gotCaller = callerFromContext(r.Context())
					a.Logger(r.Context()).Info("materialize route", zap.String("test", tt.name))
					gotRunAccess = lazyRunFromContext(r.Context()).materialized().callerAccess
					return nil
				},
				Access:      AccessPublic,
				Description: "Inspect the request context",
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/context", nil)
			if tt.header != "" {
				r.Header.Set("X-Caller-Access", tt.header)
			}
			a.Handler().ServeHTTP(w, r)

			if gotAgent != a {
				t.Fatalf("AgentFromContext() = %p, want %p", gotAgent, a)
			}
			if gotCaller.Access != tt.wantAccess {
				t.Errorf("callerFromContext().Access = %q, want %q", gotCaller.Access, tt.wantAccess)
			}
			if gotRunAccess != tt.wantAccess {
				t.Errorf("materialized run access = %q, want %q", gotRunAccess, tt.wantAccess)
			}
			if got := completedRun(t, mock); got.Status != "success" {
				t.Errorf("completion status = %q, want success", got.Status)
			}
		})
	}
}

func TestRouteMaterializedRunCompletion(t *testing.T) {
	tests := []struct {
		name       string
		handler    func(*Agent, http.ResponseWriter, *http.Request) error
		wantCode   int
		wantStatus string
		wantTrace  bool
	}{
		{
			name: "success",
			handler: func(a *Agent, w http.ResponseWriter, r *http.Request) error {
				a.Logger(r.Context()).Info("route run")
				return nil
			},
			wantCode: http.StatusOK, wantStatus: "success",
		},
		{
			name: "returned error",
			handler: func(a *Agent, w http.ResponseWriter, r *http.Request) error {
				a.Logger(r.Context()).Info("route run")
				return errors.New("route failed")
			},
			wantCode: http.StatusInternalServerError, wantStatus: "error",
		},
		{
			name: "panic",
			handler: func(a *Agent, w http.ResponseWriter, r *http.Request) error {
				a.Logger(r.Context()).Info("route run")
				panic("route panic")
			},
			wantCode: http.StatusInternalServerError, wantStatus: "error", wantTrace: true,
		},
		{
			name: "explicit 5xx",
			handler: func(a *Agent, w http.ResponseWriter, r *http.Request) error {
				a.Logger(r.Context()).Info("route run")
				w.WriteHeader(http.StatusServiceUnavailable)
				return nil
			},
			wantCode: http.StatusServiceUnavailable, wantStatus: "error",
		},
		{
			name: "explicit 4xx",
			handler: func(a *Agent, w http.ResponseWriter, r *http.Request) error {
				a.Logger(r.Context()).Info("route run")
				w.WriteHeader(http.StatusBadRequest)
				return nil
			},
			wantCode: http.StatusBadRequest, wantStatus: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			a.RegisterRoute(&Route{
				Method: http.MethodGet,
				Path:   "/run",
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					return tt.handler(a, w, r)
				},
				Access:      AccessPublic,
				Description: "Exercise route completion",
			})

			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/run", nil))

			if w.Code != tt.wantCode {
				t.Errorf("response status = %d, want %d", w.Code, tt.wantCode)
			}
			got := completedRun(t, mock)
			if got.Status != tt.wantStatus {
				t.Errorf("completion status = %q, want %q", got.Status, tt.wantStatus)
			}
			if (got.PanicTrace != "") != tt.wantTrace {
				t.Errorf("panic trace present = %t, want %t", got.PanicTrace != "", tt.wantTrace)
			}
		})
	}
}

func TestDirectToolCallerAndMaterializedRunCompletion(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		behavior   string
		wantAccess Access
		wantStatus string
		wantTrace  bool
	}{
		{name: "anonymous success", wantAccess: AccessPublic, wantStatus: "success"},
		{name: "known caller success", header: string(AccessUser), wantAccess: AccessUser, wantStatus: "success"},
		{name: "returned error", header: string(AccessUser), behavior: "error", wantAccess: AccessUser, wantStatus: "error"},
		{name: "panic", header: string(AccessUser), behavior: "panic", wantAccess: AccessUser, wantStatus: "error", wantTrace: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			var gotAgent *Agent
			var gotCaller caller
			var gotRunAccess Access
			a.RegisterTool(greetTool("direct", "Direct test tool.", func(ctx context.Context, in greetIn) (greetOut, error) {
				gotAgent = AgentFromContext(ctx)
				gotCaller = callerFromContext(ctx)
				a.Logger(ctx).Info("materialize direct tool")
				gotRunAccess = lazyRunFromContext(ctx).materialized().callerAccess
				switch tt.behavior {
				case "error":
					return greetOut{}, errors.New("tool failed")
				case "panic":
					panic("tool panic")
				}
				return greetOut{Greeting: "hello"}, nil
			}), AccessPublic)

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/__air/tool/direct", strings.NewReader(`{"name":"test"}`))
			if tt.header != "" {
				r.Header.Set("X-Caller-Access", tt.header)
			}
			a.Handler().ServeHTTP(w, r)

			if gotAgent != a {
				t.Fatalf("AgentFromContext() = %p, want %p", gotAgent, a)
			}
			if gotCaller.Access != tt.wantAccess {
				t.Errorf("callerFromContext().Access = %q, want %q", gotCaller.Access, tt.wantAccess)
			}
			if gotRunAccess != tt.wantAccess {
				t.Errorf("materialized run access = %q, want %q", gotRunAccess, tt.wantAccess)
			}
			got := completedRun(t, mock)
			if got.Status != tt.wantStatus {
				t.Errorf("completion status = %q, want %q", got.Status, tt.wantStatus)
			}
			if (got.PanicTrace != "") != tt.wantTrace {
				t.Errorf("panic trace present = %t, want %t", got.PanicTrace != "", tt.wantTrace)
			}
		})
	}
}

func TestDirectToolMissingCallerCannotInvokeUserTool(t *testing.T) {
	a, mock := testAgent(t)
	executed := false
	a.RegisterTool(greetTool("user_tool", "User-only tool.", func(ctx context.Context, in greetIn) (greetOut, error) {
		executed = true
		return greetOut{}, nil
	}), AccessUser)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/__air/tool/user_tool", strings.NewReader(`{}`))
	a.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("response status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if executed {
		t.Error("user tool executed for an anonymous caller")
	}
	if got := mock.RequestsByPath("/api/agent/run/create"); len(got) != 0 {
		t.Errorf("created %d runs for rejected tool, want 0", len(got))
	}
	if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
		t.Errorf("completed %d runs for rejected tool, want 0", len(got))
	}
}

func completedRun(t *testing.T, mock *mockairlock.Mock) wire.RunCompleteRequest {
	t.Helper()
	reqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(reqs) != 1 {
		t.Fatalf("completion requests = %d, want exactly 1", len(reqs))
	}
	var got wire.RunCompleteRequest
	if err := json.Unmarshal(reqs[0].Body, &got); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	return got
}
