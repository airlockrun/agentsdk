package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/internal/mockairlock"
	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandlerHostDelivery(t *testing.T) {
	for _, family := range []string{"route", "standalone route", "webhook", "job", "runtime invoke", "refresh", "static", "bundled asset"} {
		t.Run(family, func(t *testing.T) {
			a, mock := testAgent(t)
			var executed atomic.Int32
			user := User{ID: uuid.NewString(), Email: "user@example.com", DisplayName: "Test User", PlatformMember: true}
			runID, bridgeID := uuid.NewString(), uuid.NewString()
			invocationToken := strings.Repeat("45", 32)
			a.RegisterRoute(&Route{
				Method: http.MethodPost, Path: "/probe/{id}", Access: AccessPublic, Description: "Inspect delivery",
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					executed.Add(1)
					got, ok := CallerFromContext(r.Context()).User()
					if !ok || got != user || CallerFromContext(r.Context()).Access() != AccessAdmin || r.PathValue("id") != "value" {
						t.Errorf("lost route attribution: user=%+v", got)
					}
					if r.Header.Get("Authorization") != "Bearer "+a.token || r.Header.Get("X-Custom-Auth") != "application-value" {
						t.Error("delivery verification rewrote application headers")
					}
					if family == "route" && (runFromContext(r.Context()).id != runID || runFromContext(r.Context()).invocationToken != invocationToken) {
						t.Error("lost ingress run")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != `{"name":"test"}` {
						t.Errorf("changed route body: %q, %v", body, err)
					}
					return nil
				},
			})
			a.RegisterWebhook(&Webhook{Path: "event", Verify: WebhookVerificationNone, Description: "Receive event",
				Handler: func(ctx context.Context, body []byte, _ *EventWriter) error {
					executed.Add(1)
					active := runFromContext(ctx)
					if _, ok := CallerFromContext(ctx).User(); ok || active.id != runID || active.invocationToken != invocationToken || active.bridgeID != bridgeID || string(body) != `{"name":"test"}` {
						t.Error("incorrect webhook attribution or payload")
					}
					return nil
				},
			})
			runtime := runtimeRequest(a, "tool//inspect")
			runtime.Context.Caller.User.ID = user.ID
			a.RegisterTool(tool.New("inspect").Description("Inspect caller").Execute(func(ctx context.Context, _ json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
				executed.Add(1)
				got, ok := CallerFromContext(ctx).User()
				caller := callScopeFromContext(ctx)
				if !ok || got.ID != user.ID {
					t.Errorf("lost tool user: %+v", got)
				}
				if family == "runtime invoke" {
					if active := runFromContext(ctx); active.id != runtime.Context.RunID || caller.Access != AccessUser {
						t.Error("headers overrode runtime body attribution")
					}
				}
				return tool.Result{Output: `{"ok":true}`}, nil
			}).Build(), AccessUser)
			job := testJobDefinition(1)
			job.Handler = func(ctx context.Context, _ JobContext, _ testJobInput) (testJobOutput, error) {
				executed.Add(1)
				if active := runFromContext(ctx); active.id != runID || active.invocationToken != invocationToken || active.userID != validJobRunRequest(a).Caller.User.ID {
					t.Error("incorrect job attribution")
				}
				return testJobOutput{Result: "ok"}, nil
			}
			RegisterJob(a, job)
			a.RegisterStaticAsset(&StaticAsset{Name: "app.css", ContentType: "text/css", Data: []byte("body{}")})
			method, path, body := http.MethodPost, "/probe/value", `{"name":"test"}`
			wantExecuted, wantRequests, wantStatus := int32(1), 0, http.StatusOK
			switch family {
			case "route":
				wantRequests = 1
			case "webhook":
				path, wantRequests = "/webhook/event", 1
			case "job":
				data, err := json.Marshal(validJobRunRequest(a))
				if err != nil {
					t.Fatal(err)
				}
				path, body, wantRequests = "/job/convert_video/1", string(data), 1
			case "runtime invoke":
				data, err := json.Marshal(runtime)
				if err != nil {
					t.Fatal(err)
				}
				path, body = wire.RuntimeInvokePath, string(data)
			case "refresh":
				path, wantExecuted, wantRequests, wantStatus = "/refresh", 0, 1, http.StatusNoContent
			case "static":
				method, path, wantExecuted = http.MethodGet, "/static/app.css", 0
			case "bundled asset":
				method, path, wantExecuted = http.MethodGet, Assets.HTMX, 0
			}
			server := httptest.NewServer(a.Handler())
			defer server.Close()
			for _, auth := range []struct {
				name   string
				values []string
				valid  bool
			}{
				{name: "missing with forged attribution"},
				{name: "wrong token", values: []string{"Bearer another-app-token"}},
				{name: "wrong scheme", values: []string{"Basic " + a.token}},
				{name: "missing bearer", values: []string{a.token}},
				{name: "empty bearer", values: []string{"Bearer"}},
				{name: "comma joined", values: []string{"Bearer " + a.token + ", Bearer forged"}},
				{name: "duplicate valid", values: []string{"Bearer " + a.token, "Bearer " + a.token}},
				{name: "valid then forged", values: []string{"Bearer " + a.token, "Bearer forged"}},
				{name: "forged then valid", values: []string{"Bearer forged", "Bearer " + a.token}},
				{name: "valid delivery", values: []string{"Bearer " + a.token}, valid: true},
			} {
				t.Run(auth.name, func(t *testing.T) {
					req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					for i, value := range auth.values {
						name := "Authorization"
						if i != 0 {
							name = "aUtHoRiZaTiOn"
						}
						req.Header[name] = append(req.Header[name], value)
					}
					req.Header.Set("X-User-ID", user.ID)
					req.Header.Set("X-User-Email", user.Email)
					req.Header.Set("X-User-Name", user.DisplayName)
					req.Header.Set("X-Caller-Access", string(AccessAdmin))
					metadata := testWireCaller("user", wire.AccessAdmin)
					*metadata.User = wire.CallerUser(user)
					if family == "webhook" {
						metadata = testWireCaller("application", wire.AccessAdmin)
					}
					setTestCallerHeader(t, req, metadata)
					if family != "standalone route" {
						req.Header.Set("X-Run-ID", runID)
						req.Header.Set(wire.InvocationTokenHeader, invocationToken)
					}
					req.Header.Set("X-Bridge-ID", bridgeID)
					req.Header.Set(jobLeaseTokenHeader, testJobLeaseToken)
					req.Header.Set("X-Custom-Auth", "application-value")
					resp, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					data, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					status, calls, requests := http.StatusUnauthorized, int32(0), 0
					if auth.valid {
						status, calls, requests = wantStatus, wantExecuted, wantRequests
					}
					if resp.StatusCode != status || executed.Load() != calls || len(mock.Requests()) != requests {
						t.Fatalf("status=%d calls=%d platform requests=%d; want %d/%d/%d; body=%s", resp.StatusCode, executed.Load(), len(mock.Requests()), status, calls, requests, data)
					}
				})
			}
		})
	}
}

func TestHandlerHealthException(t *testing.T) {
	a, mock := testAgent(t)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodHead, "/health", http.StatusOK},
		{http.MethodPost, "/health", http.StatusUnauthorized},
		{http.MethodGet, "/health/", http.StatusUnauthorized},
		{http.MethodGet, "/health/../static/app.css", http.StatusUnauthorized},
		{http.MethodGet, "/manifest", http.StatusUnauthorized},
		{http.MethodGet, "/missing", http.StatusUnauthorized},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
	if len(mock.Requests()) != 0 {
		t.Fatal("health or rejected requests performed platform work")
	}
}

func TestHandlerRejectsAmbiguousAttribution(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/", Access: AccessPublic, Description: "Reject ambiguous delivery",
		Handler: func(http.ResponseWriter, *http.Request) error {
			t.Error("ambiguous delivery executed")
			return nil
		},
	})
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	for _, header := range []string{wire.CallerHeader, "X-Run-ID", "X-Bridge-ID", jobLeaseTokenHeader, wire.InvocationTokenHeader} {
		t.Run(header, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+a.token)
			setTestCallerHeader(t, req, testWireCaller("anonymous", wire.AccessPublic))
			req.Header.Add(header, "one")
			req.Header.Add(header, "two")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", resp.StatusCode)
			}
		})
	}
	if len(mock.Requests()) != 0 {
		t.Fatal("rejected attribution performed platform work")
	}
}

func TestRouteCallerHeaderIsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		auth   bool
		status int
	}{
		{"missing", "", true, http.StatusBadRequest},
		{"malformed", "invalid", true, http.StatusBadRequest},
		{"authenticate before parsing", "invalid", false, http.StatusUnauthorized},
		{"anonymous ignores flat headers and outer user", "anonymous", true, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, mock := testAgent(t)
			executed := false
			a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/caller", Access: AccessPublic, Description: "Inspect caller",
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					executed = true
					caller := CallerFromContext(r.Context())
					if caller.Kind() != CallerAnonymous || caller.Access() != AccessPublic {
						t.Fatal("forged actor or access")
					}
					if _, ok := caller.User(); ok {
						t.Fatal("forged user")
					}
					if _, ok := caller.Initiator(); ok {
						t.Fatal("forged initiator")
					}
					if callScopeFromContext(r.Context()).Access != AccessPublic {
						t.Fatal("forged private access")
					}
					w.WriteHeader(http.StatusNoContent)
					return nil
				}})
			outer := newRun(a, "outer", "", "", t.Context())
			outer.setCaller(callerFromWire(testWireCaller("user", wire.AccessAdmin)))
			r := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(outer.checkedCtx())
			if tc.auth {
				r.Header.Set("Authorization", "Bearer "+a.token)
			}
			if tc.header == "anonymous" {
				setTestCallerHeader(t, r, testWireCaller("anonymous", wire.AccessPublic))
			} else if tc.header != "" {
				r.Header.Set(wire.CallerHeader, tc.header)
			}
			for _, name := range []string{"X-User-ID", "X-User-Email", "X-User-Name", "X-Caller-Access"} {
				r.Header.Add(name, "admin")
				r.Header.Add(name, "forged")
			}
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || executed != (tc.status == http.StatusNoContent) {
				t.Fatalf("status=%d executed=%t body=%s", w.Code, executed, w.Body.String())
			}
			if len(mock.Requests()) != 0 {
				t.Fatal("caller lookup made network requests")
			}
		})
	}
}

func TestHandlerRequiresToken(t *testing.T) {
	for _, token := range []string{"", " \t"} {
		t.Run(token, func(t *testing.T) {
			a, _ := testAgent(t)
			a.token = token
			expectPanicContains(t, "Handler requires AIRLOCK_AGENT_TOKEN", func() { a.Handler() })
		})
	}
}

func TestHandlerCallerContextDoesNotAuthenticateDelivery(t *testing.T) {
	a, mock := testAgent(t)
	ctx := testcaller.With(context.Background(), testWireCaller("user", wire.AccessAdmin))
	r := httptest.NewRequest(http.MethodPost, "/refresh", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || len(mock.Requests()) != 0 {
		t.Fatalf("test caller bypassed authentication: status=%d requests=%d", w.Code, len(mock.Requests()))
	}
}

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

func TestHandlerDoesNotHostChat(t *testing.T) {
	a, mock := testAgent(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"message":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+a.token)
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("/prompt status=%d", w.Code)
	}
	if len(mock.Requests()) != 0 {
		t.Fatal("unhandled chat route performed runtime work")
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
			var gotCaller callScope
			var gotRunAccess Access
			a.RegisterRoute(&Route{
				Method: http.MethodGet,
				Path:   "/context",
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					gotAgent = AgentFromContext(r.Context())
					gotCaller = callScopeFromContext(r.Context())
					a.Logger(r.Context()).Info("materialize route", zap.String("test", tt.name))
					gotRunAccess = lazyRunFromContext(r.Context()).materialized().callerAccess
					return nil
				},
				Access:      AccessPublic,
				Description: "Inspect the request context",
			})

			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/context", nil)
			r.Header.Set("Authorization", "Bearer "+a.token)
			metadata := testWireCaller("anonymous", wire.AccessPublic)
			if tt.header != "" {
				metadata = testWireCaller("user", wire.Access(tt.header))
			}
			setTestCallerHeader(t, r, metadata)
			if tt.header != "" {
				r.Header.Set("X-Caller-Access", tt.header)
			}
			a.Handler().ServeHTTP(w, r)

			if gotAgent != a {
				t.Fatalf("AgentFromContext() = %p, want %p", gotAgent, a)
			}
			if gotCaller.Access != tt.wantAccess {
				t.Errorf("callScopeFromContext().Access = %q, want %q", gotCaller.Access, tt.wantAccess)
			}
			if gotRunAccess != AccessPublic {
				t.Errorf("app-owned run access = %q, want %q", gotRunAccess, AccessPublic)
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
			r := httptest.NewRequest(http.MethodGet, "/run", nil)
			r.Header.Set("Authorization", "Bearer "+a.token)
			setTestCallerHeader(t, r, testWireCaller("anonymous", wire.AccessPublic))
			a.Handler().ServeHTTP(w, r)

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

func TestDirectToolRouteIsNotRegistered(t *testing.T) {
	a, mock := testAgent(t)
	executed := false
	a.RegisterTool(greetTool("user_tool", "User-only tool.", func(ctx context.Context, in greetIn) (greetOut, error) {
		executed = true
		return greetOut{}, nil
	}), AccessUser)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/__air/tool/user_tool", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+a.token)
	a.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("response status = %d, want %d", w.Code, http.StatusNotFound)
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

func TestHTTPRunCompletionFailureLogged(t *testing.T) {
	logger := agentLogger()
	defer func() { baseLogger = logger }()
	for _, tc := range []struct {
		name        string
		status      int
		dispatchErr error
		wantStatus  string
	}{
		{"success", http.StatusOK, nil, "success"},
		{"quiet client error", http.StatusBadRequest, nil, "success"},
		{"server error", http.StatusInternalServerError, nil, "error"},
		{"handler error", http.StatusOK, errors.New("handler failed"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, mock := testAgent(t)
			mock.RunCompleteStatus = http.StatusConflict
			core, logs := observer.New(zap.ErrorLevel)
			baseLogger = zap.New(core)
			run := newRun(a, "completion-run", "", "", t.Context())
			completeHTTPRun(t.Context(), run, tc.status, tc.dispatchErr, "")
			got := completedRun(t, mock)
			if got.Status != tc.wantStatus {
				t.Fatalf("status=%s, want %s", got.Status, tc.wantStatus)
			}
			entries := logs.FilterMessage("record HTTP run completion failed").All()
			if len(entries) != 1 || entries[0].ContextMap()["run_id"] != run.id || entries[0].ContextMap()["error"] == nil {
				t.Fatalf("completion failure not logged with run and error: %+v", entries)
			}
		})
	}
}

func TestRouteCompletionAfterContentLengthEOF(t *testing.T) {
	a, _ := testAgent(t)
	completed := make(chan wire.RunCompleteRequest, 1)
	deliveryClosed := make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/run/complete" || r.Header.Get(wire.InvocationTokenHeader) != strings.Repeat("ab", 32) {
			t.Errorf("unexpected completion request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var completion wire.RunCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&completion); err != nil {
			t.Error(err)
		}
		select {
		case <-deliveryClosed:
		default:
			t.Error("completion preceded response EOF")
		}
		completed <- completion
	}))
	defer host.Close()
	a.client = newAirlockClient(host.URL, a.token, host.Client())
	release := make(chan struct{})
	a.RegisterRoute(&Route{
		Method: http.MethodGet, Path: "/early", Access: AccessPublic, Description: "Complete after delivery",
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Length", "2")
			if _, err := io.WriteString(w, "ok"); err != nil {
				return err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			<-release
			a.Logger(r.Context()).Info("final bookkeeping after EOF")
			return nil
		},
	})
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/early", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("X-Run-ID", "early-run")
	setTestCallerHeader(t, req, testWireCaller("anonymous", wire.AccessPublic))
	req.Header.Set(wire.InvocationTokenHeader, strings.Repeat("ab", 32))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "ok" || resp.ContentLength != 2 {
		t.Fatalf("response: body=%q length=%d error=%v", body, resp.ContentLength, err)
	}
	select {
	case <-completed:
		t.Fatal("handler completed before final bookkeeping")
	default:
	}
	close(deliveryClosed)
	release <- struct{}{}
	select {
	case completion := <-completed:
		if completion.RunID != "early-run" || completion.Status != "success" || len(completion.Logs) != 1 || completion.Logs[0].Message != "final bookkeeping after EOF" {
			t.Fatalf("completion lost final bookkeeping: %+v", completion)
		}
	case <-ctx.Done():
		t.Fatal("completion not recorded after EOF")
	}
}

func setTestCallerHeader(t *testing.T, r *http.Request, c wire.Caller) {
	t.Helper()
	value, err := wire.EncodeCallerHeader(c)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(wire.CallerHeader, value)
}
