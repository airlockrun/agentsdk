package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

func TestAppRunReceiptPropagatesToDetachedCompletion(t *testing.T) {
	token := strings.Repeat("34", 32)
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/run/create" {
			_ = json.NewEncoder(w).Encode(wire.CreateRunResponse{RunID: "app-run", InvocationToken: token, Caller: testWireCaller("application", wire.AccessPublic)})
			return
		}
		requests <- r.Clone(context.Background())
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	a := &Agent{client: newAirlockClient(server.URL, "app-token", server.Client())}
	run := a.newRunFromAirlock(context.Background(), "background", "")
	if run.invocationToken != token {
		t.Fatal("creation receipt was not retained")
	}
	if err := run.complete(context.Background(), "success", "", "", ""); err != nil {
		t.Fatal(err)
	}
	req := <-requests
	if req.Header.Get(wire.InvocationTokenHeader) != token || req.Header.Get("X-Airlock-Run-ID") != "app-run" {
		t.Fatal("detached completion lost authority")
	}
}

func TestAppRunCreationDoesNotSendCallerAuthority(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterRoute(&Route{Method: http.MethodGet, Path: "/work", Access: AccessPublic, Description: "Run app work", Handler: func(w http.ResponseWriter, r *http.Request) error {
		active := a.runForCall(r.Context())
		if active.userID != "" || active.conversationID != "" || active.callerAccess != AccessPublic {
			t.Fatalf("app-created run borrowed caller authority: %+v", active)
		}
		caller := CallerFromContext(active.ctx)
		if caller.Kind() != CallerApplication {
			t.Fatal("native run is not application-owned")
		}
		if _, ok := caller.User(); ok {
			t.Fatal("native run inherited a user")
		}
		if _, ok := caller.Initiator(); ok {
			t.Fatal("native run inherited an initiator")
		}
		if CallerFromContext(r.Context()).Kind() != CallerUser {
			t.Fatal("materialization changed the route caller")
		}
		return nil
	}})
	r := httptest.NewRequest(http.MethodGet, "/work", nil)
	r.Header.Set("Authorization", "Bearer test-token")
	setTestCallerHeader(t, r, testWireCaller("user", wire.AccessAdmin))
	r.Header.Set("X-Caller-Access", "admin")
	r.Header.Set("X-User-ID", "caller-user")
	r.Header.Set("X-Conversation-ID", "caller-conversation")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	created := 0
	for _, req := range mock.Requests() {
		if req.Path != "/api/agent/run/create" {
			continue
		}
		created++
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(req.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 2 || payload["triggerType"] == nil || payload["triggerRef"] == nil {
			t.Fatalf("run creation carries unexpected fields: %s", req.Body)
		}
		if req.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing app credential")
		}
	}
	if created != 1 {
		t.Fatalf("created %d runs, want one", created)
	}
}

func TestBackgroundCallerDoesNotInheritTestUser(t *testing.T) {
	a, _ := testAgent(t)
	ctx := testcaller.With(t.Context(), testWireCaller("user", wire.AccessAdmin))
	r := a.runForCall(ctx)
	c := CallerFromContext(r.ctx)
	if c.Kind() != CallerApplication {
		t.Fatal("background inherited outer user")
	}
	if _, ok := c.Initiator(); ok {
		t.Fatal("background inherited outer initiator")
	}
	if CallerFromContext(ctx).Kind() != CallerUser {
		t.Fatal("background changed input context")
	}
}

func TestNativeRunRequiresHostCaller(t *testing.T) {
	for _, kind := range []string{"missing", "user", "application"} {
		t.Run(kind, func(t *testing.T) {
			metadata := testWireCaller(kind, wire.AccessAdmin)
			if kind == "missing" {
				metadata = wire.Caller{}
			}
			metadata.Origin = wire.CallerOrigin{Interface: "application", Platform: "native", Execution: "background"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(wire.CreateRunResponse{RunID: "run", Caller: metadata})
			}))
			defer server.Close()
			a := &Agent{client: newAirlockClient(server.URL, "app-token", server.Client())}
			if kind != "application" {
				defer func() {
					if recover() == nil {
						t.Fatal("invalid native caller did not panic")
					}
				}()
			}
			r := a.newRunFromAirlock(t.Context(), "background", "")
			if CallerFromContext(r.ctx) != callerFromWire(metadata) {
				t.Fatal("native caller differs from host response")
			}
		})
	}
}

func TestRunCompletionCarriesJobAttempt(t *testing.T) {
	a, mock := testAgent(t)
	ctx := contextWithJobRun(context.Background(), &jobRunContext{agent: a, id: "job-id", attempt: 3, leaseToken: "attempt-token"})
	r := newRun(a, "run-id", "", "", ctx)
	if err := r.complete(ctx, "success", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, req := range mock.Requests() {
		if req.Path != "/api/agent/run/complete" {
			continue
		}
		var completion wire.RunCompleteRequest
		if err := json.Unmarshal(req.Body, &completion); err != nil {
			t.Fatal(err)
		}
		if completion.JobID != "job-id" || completion.Attempt != 3 || completion.LeaseToken != "attempt-token" {
			t.Fatalf("missing attempt proof: %+v", completion)
		}
		return
	}
	t.Fatal("missing completion")
}
