package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

type platformFunc func(context.Context, chatruntime.Invocation) (tool.Result, error)

func (f platformFunc) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	return f(ctx, in)
}

func TestChatInvokesAppUnderBorrowedRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/chat\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	runID := uuid.NewString()
	env := agenttest.New(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "chat test"})
		a.RegisterTool(tool.New("lookup").Description("Read the app database").SchemaFromStruct(struct{}{}).
			Execute(func(ctx context.Context, _ json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
				var answer int
				if err := a.DB().QueryRowContext(ctx, "SELECT 42").Scan(&answer); err != nil {
					return tool.Result{}, err
				}
				a.Logger(ctx).Info("database lookup")
				return tool.Result{Output: `{"answer":42}`, Title: "Database answer", Metadata: map[string]any{"source": "app"}}, nil
			}).Build(), agentsdk.AccessUser)
		return a
	})
	defs, err := capability.Catalog(env.Agent.Manifest(), capability.Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	var selected []capability.Definition
	for _, d := range defs {
		if d.Path.ID() == "tool//lookup" {
			selected = append(selected, d)
		}
	}
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "direct", false: "deno"}[direct], func(t *testing.T) {
			call := testutil.MockToolCallResponse("lookup-call", "tool__lookup", map[string]any{}, testutil.MockUsage(10, 10))
			var factory chatruntime.ExecutorFactory
			if !direct {
				image := os.Getenv("TEST_JSEXEC_IMAGE")
				if image == "" {
					image = "agentsdk-jsexec-test:local"
					if err := jsexec.BuildImage(t.Context(), image); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				factory, err = agenttest.Executor(agenttest.ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits()})
				if err != nil {
					t.Fatal(err)
				}
				call = testutil.MockToolCallResponse("lookup-call", "run_js", map[string]any{"code": "return await tools.lookup({});", "description": "Read the app database"}, testutil.MockUsage(10, 10))
			}
			model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{call, testutil.MockTextResponse("The answer is 42", testutil.MockUsage(10, 10))}})
			env.Airlock.Reset()
			user := &wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}
			caller := wire.Caller{Kind: "user", Access: wire.AccessUser, User: user, Initiator: user, Origin: wire.CallerOrigin{Interface: "chat", Execution: "request"}}
			result, err := env.Chat(t.Context(), wire.RuntimeContext{AgentID: uuid.Nil.String(), RunID: runID, Caller: caller, InvocationToken: strings.Repeat("12", 32)}, chatruntime.Input{
				Message: "Look up the answer", Model: model, ModelLimits: session.ModelLimits{Input: 80000}, MaxSteps: 5,
				SessionStore: &agenttest.MemoryStore{}, Sink: &agenttest.Events{}, Capabilities: selected, DirectTools: direct, ExecutorFactory: factory,
				Backend: platformFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
					return tool.Result{}, errors.New("unexpected platform call")
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.Status != sol.RunCompleted || result.Run.TotalText != "The answer is 42" || len(result.AppCalls) != 1 {
				t.Fatalf("result=%+v", result)
			}
			if len(result.AppCalls[0].Logs) != 1 || result.AppCalls[0].Title != "Database answer" {
				t.Fatalf("app telemetry=%+v", result.AppCalls)
			}
			for _, req := range env.Airlock.Requests() {
				if strings.HasPrefix(req.Path, "/api/agent/run") {
					t.Fatalf("borrowed chat created/completed a run: %+v", req)
				}
			}
		})
	}
}
