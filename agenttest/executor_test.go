package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/session"
	"github.com/testcontainers/testcontainers-go"
)

func TestExecutorInjectedTransportIsLazy(t *testing.T) {
	opens := 0
	sentinel := errors.New("injected transport")
	factory, err := agenttest.Executor(agenttest.ExecutorConfig{Limits: jsexec.DefaultLimits(), OpenTransport: func(context.Context) (io.ReadWriteCloser, error) { opens++; return nil, sentinel }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		response  []stream.Event
		suspended bool
	}{
		{"text", testutil.MockTextResponse("hello", testutil.MockUsage(10, 10)), false},
		{"approval", testutil.MockToolCallResponse("gate", "run_js", map[string]any{"code": "return 42", "description": "Calculate answer", "request_confirmation": true}, testutil.MockUsage(10, 10)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := chatruntime.Input{Message: "hello", Model: testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponse: tc.response}), ModelLimits: session.ModelLimits{Input: 80000}, MaxSteps: 5, SessionStore: &agenttest.MemoryStore{}, Sink: &agenttest.Events{}, ExecutorFactory: factory,
				Backend: platformFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
					return tool.Result{}, errors.New("unexpected backend call")
				})}
			result, err := chatruntime.Run(t.Context(), in)
			if err != nil || (result.Status == sol.RunSuspended) != tc.suspended {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if opens != 0 {
				t.Fatal("unused executor opened a transport")
			}
		})
	}
	if _, err := factory(t.Context(), nil); !errors.Is(err, sentinel) || opens != 1 {
		t.Fatalf("injected transport: opens=%d err=%v", opens, err)
	}
}

func TestExecutorDenoSharedChat(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	image := os.Getenv("TEST_JSEXEC_IMAGE")
	if image == "" {
		image = "agentsdk-jsexec-test:local"
		if err := jsexec.BuildImage(ctx, image); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := agenttest.Executor(agenttest.ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits(), User: &jsexec.User{ID: "caller-123", Email: "alice@example.com", DisplayName: "Alice"}})
	if err != nil {
		t.Fatal(err)
	}
	def := capability.Definition{Path: capability.Local(capability.Tool, "", "double"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`)}
	var mu sync.Mutex
	var calls []string
	backend := platformFunc(func(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
		mu.Lock()
		calls = append(calls, in.ToolCallID)
		mu.Unlock()
		var args struct {
			Value int `json:"value"`
		}
		if err := json.Unmarshal(in.Input, &args); err != nil {
			return tool.Result{}, err
		}
		out, _ := json.Marshal(map[string]int{"value": args.Value * 2})
		return tool.Result{Output: string(out)}, nil
	})
	js := func(id, code string) []stream.Event {
		return testutil.MockToolCallResponse(id, "run_js", map[string]any{"code": code, "description": "Calculate doubled values"}, testutil.MockUsage(10, 10))
	}
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{
		js("first", `const values = await Promise.all([tools.double({value: 10}), tools.double({value: 11})]); globalThis.total = values[0].value + values[1].value; console.log("calculated"); return globalThis.total;`),
		js("second", `air.log("caller", user.displayName); return {total: globalThis.total, type: typeof values, user};`),
		testutil.MockTextResponse("42", testutil.MockUsage(10, 10)),
	}})
	result, err := chatruntime.Run(ctx, chatruntime.Input{Message: "Calculate", Model: model, ModelLimits: session.ModelLimits{Input: 80000}, MaxSteps: 5, SessionStore: &agenttest.MemoryStore{}, Sink: &agenttest.Events{}, Backend: backend, Capabilities: []capability.Definition{def}, ExecutorFactory: factory})
	if err != nil || result.Status != sol.RunCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	encoded, _ := json.Marshal(model.DoStreamCalls[2])
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "first" {
		t.Fatalf("attributed callbacks=%v transcript=%s", calls, encoded)
	}
	for _, want := range []string{"calculated", "total", "42", "undefined", "caller Alice", "caller-123", "alice@example.com"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("real Deno result missing %q: %s", want, encoded)
		}
	}
}
