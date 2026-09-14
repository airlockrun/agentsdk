package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader consumed before runtime check") }

func TestOnStartRunsInRegistrationOrder(t *testing.T) {
	a, _ := testAgent(t)
	var got []string
	a.OnStart("first", func(context.Context) error {
		got = append(got, "first")
		return nil
	})
	a.OnStart("second", func(context.Context) error {
		got = append(got, "second")
		return nil
	})
	if err := a.runStartHooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("startup order = %v, want %v", got, want)
	}
}

func TestOnStartCaller(t *testing.T) {
	a, mock := testAgent(t)
	outer := newRun(a, "outer-run", "", "", t.Context())
	outer.setCaller(callerFromWire(testWireCaller("user", wire.AccessUser)))
	ctx := outer.checkedCtx()
	a.OnStart("inspect", func(ctx context.Context) error {
		caller := CallerFromContext(ctx)
		if caller.Kind() != CallerApplication || caller.Access() != AccessAdmin || caller.Origin() != (Origin{Interface: InterfaceApplication, Execution: ExecutionStartup}) {
			t.Fatalf("startup caller = %+v", caller)
		}
		if _, ok := caller.User(); ok {
			t.Fatal("startup inherited acting user")
		}
		if _, ok := caller.Initiator(); ok {
			t.Fatal("startup inherited initiator")
		}
		if AgentFromContext(ctx) != a || runFromContext(ctx) != nil {
			t.Fatal("startup binding is not independent")
		}
		return nil
	})
	if err := a.runStartHooks(ctx); err != nil {
		t.Fatal(err)
	}
	if CallerFromContext(ctx).Kind() != CallerUser {
		t.Fatal("startup changed input context")
	}
	if len(mock.Requests()) != 0 {
		t.Fatal("startup lookup performed I/O")
	}
}

func TestOnStartFailsLoudly(t *testing.T) {
	a, _ := testAgent(t)
	want := errors.New("hydrate failed")
	a.OnStart("hydrate", func(context.Context) error { return want })
	err := a.runStartHooks(t.Context())
	if !errors.Is(err, want) || err.Error() != `agentsdk: startup hook "hydrate": hydrate failed` {
		t.Fatalf("startup error = %v", err)
	}
}

func TestOnStartCompletesMaterializedRun(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			a, mock := testAgent(t)
			failure := errors.New("startup failed")
			a.OnStart("logged", func(ctx context.Context) error {
				a.Logger(ctx).Info("startup log")
				if CallerFromContext(ctx).Origin().Execution != ExecutionStartup {
					t.Fatal("materialization changed startup caller")
				}
				switch outcome {
				case "error":
					return failure
				case "panic":
					panic(failure)
				}
				return nil
			})
			ctx := contextWithJobRun(t.Context(), &jobRunContext{agent: a, id: "outer-job", attempt: 1, leaseToken: "outer-lease"})
			var gotErr error
			var gotPanic any
			func() {
				defer func() { gotPanic = recover() }()
				gotErr = a.runStartHooks(ctx)
			}()
			if outcome == "error" && !errors.Is(gotErr, failure) || outcome != "error" && gotErr != nil {
				t.Fatalf("error = %v", gotErr)
			}
			if outcome == "panic" && gotPanic != failure || outcome != "panic" && gotPanic != nil {
				t.Fatalf("panic = %v", gotPanic)
			}
			created, completed := 0, 0
			for _, req := range mock.Requests() {
				switch req.Path {
				case "/api/agent/run/create":
					created++
				case "/api/agent/run/complete":
					completed++
					var body wire.RunCompleteRequest
					if err := json.Unmarshal(req.Body, &body); err != nil {
						t.Fatal(err)
					}
					wantStatus := "success"
					if outcome != "success" {
						wantStatus = "error"
					}
					if body.Status != wantStatus || len(body.Logs) != 1 || body.Logs[0].Message != "startup log" {
						t.Fatalf("completion = %+v", body)
					}
					if body.JobID != "" || body.Attempt != 0 || body.LeaseToken != "" {
						t.Fatal("startup completion inherited job authority")
					}
					if (body.PanicTrace != "") != (outcome == "panic") {
						t.Fatal("startup panic trace mismatch")
					}
				}
			}
			if created != 1 || completed != 1 {
				t.Fatalf("created=%d completed=%d, want one of each", created, completed)
			}
		})
	}
}

func TestOnStartRejectsInvalidRegistration(t *testing.T) {
	a, _ := testAgent(t)
	expectPanicContains(t, "name is required", func() {
		a.OnStart("", func(context.Context) error { return nil })
	})
	expectPanicContains(t, "callback is required", func() {
		a.OnStart("hydrate", nil)
	})
	a.OnStart("hydrate", func(context.Context) error { return nil })
	expectPanicContains(t, "duplicate OnStart", func() {
		a.OnStart("hydrate", func(context.Context) error { return nil })
	})
}

func TestDefinitionOnlyAgentRejectsRuntimeOperationsBeforeWork(t *testing.T) {
	a := New(Config{Description: "definition only"})
	conn := a.RegisterConnection(&Connection{
		Slug: "api", Name: "API", Description: "API", BaseURL: "https://example.com",
		AuthMode: ConnectionAuthNone, Access: AccessAdmin,
	})
	mcp := a.RegisterMCP(&MCP{
		Slug: "tools", Name: "Tools", URL: "https://example.com/mcp",
		AuthMode: MCPAuthNone, Access: AccessAdmin,
	})
	topic := a.RegisterTopic(&Topic{Slug: "alerts", Description: "Alerts", Access: AccessUser, PerUser: true})
	env := a.RegisterEnvVar(&EnvVar{Slug: "api_key", Description: "API key"})

	assertUnavailable := func(operation string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), operation+" is unavailable before the agent runtime starts") {
			t.Fatalf("%s error = %v", operation, err)
		}
	}
	ctx := context.Background()
	_, err := a.WriteFile(ctx, "cache/data", panicReader{}, "application/octet-stream")
	assertUnavailable("WriteFile", err)
	_, err = conn.RequestStream(ctx, RequestOpts{Body: panicReader{}})
	assertUnavailable("ConnectionHandle.RequestStream", err)
	_, err = mcp.CallTool(ctx, "query", panicReader{})
	assertUnavailable("MCPHandle.CallTool", err)
	assertUnavailable("TopicHandle.Publish", topic.Publish(ctx, nil))
	_, err = env.Get(ctx)
	assertUnavailable("EnvVarHandle.Get", err)
	_, err = a.Seal(ctx, "secret")
	assertUnavailable("Seal", err)

	localDir := filepath.Join(t.TempDir(), "not-created")
	assertUnavailable("SyncDown", a.SyncDown(ctx, "cache", localDir))
	if _, err := os.Stat(localDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncDown touched local directory before runtime: %v", err)
	}
	expectPanicContains(t, "LLM is unavailable before the agent runtime starts", func() {
		a.LLM(ctx, "undeclared")
	})
}

func TestRuntimeUnavailableUntilDependenciesAreInitialized(t *testing.T) {
	a := New(Config{Description: "initializing"})
	if err := a.beginStart(); err != nil {
		t.Fatal(err)
	}
	if a.runtimeAvailable() {
		t.Fatal("runtime is available during dependency initialization")
	}
	a.runtimeInitialized()
	if !a.runtimeAvailable() {
		t.Fatal("runtime is unavailable after dependencies are initialized")
	}
}
