package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
)

type runInput struct {
	Text string `json:"text"`
}
type runOutput struct {
	Answer string `json:"answer"`
}

func runScope() wire.RuntimeContext {
	return wire.RuntimeContext{AgentID: uuid.Nil.String(), RunID: uuid.NewString(), InvocationToken: strings.Repeat("ab", 32),
		Caller: wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: "background"}}}
}

func runResponse(id, name, input string) testutil.MockResponse {
	return testutil.MockResponse{ToolCalls: []stream.ToolCall{{ID: id, Name: name, Input: json.RawMessage(input)}}, Usage: stream.UsageFrom(10, 5)}
}

func runUnitEnv(t *testing.T, responses ...testutil.MockResponse) (*Env, *agentsdk.AgentHandle[runInput, runOutput], RunOptions) {
	t.Helper()
	return runUnitEnvBudget(t, nil, session.ModelLimits{Context: 100000, Output: 4096}, responses...)
}

func runUnitEnvBudget(t *testing.T, budget *agentsdk.AgentBudget, limits session.ModelLimits, responses ...testutil.MockResponse) (*Env, *agentsdk.AgentHandle[runInput, runOutput], RunOptions) {
	t.Helper()
	a := agentsdk.New(agentsdk.Config{Description: "registered task test"})
	a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Task model"})
	h := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "task", Description: "A task", Instructions: "Return a typed answer.", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1, Budget: budget})
	models, err := prepareModels(t.Context(), a.Manifest(), Options{Mocks: map[string]testutil.MockConfig{"reasoning": {ID: "run-test", Responses: responses}}, Limits: map[string]session.ModelLimits{"reasoning": limits}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := Executor(ExecutorConfig{Limits: jsexec.DefaultLimits(), OpenTransport: func(context.Context) (io.ReadWriteCloser, error) { panic("completion must not open Deno") }})
	if err != nil {
		t.Fatal(err)
	}
	return &Env{Agent: a, models: models, appID: uuid.Nil.String()}, h, RunOptions{Scope: runScope(), ExecutorFactory: factory, MaxSteps: 3}
}

func TestRunAgentCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		kind        agentsdk.AgentReplyKind
	}{
		{"output", `{"kind":"output","output":{"answer":"42"}}`, agentsdk.AgentReplyOutput},
		{"question", `{"kind":"needs_input","question":"Which report?"}`, agentsdk.AgentReplyNeedsInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, h, opts := runUnitEnv(t, runResponse("complete-1", "complete", tc.reply))
			opts.Scope = wire.RuntimeContext{}
			result, err := RunAgent(t, t.Context(), e, h, runInput{Text: "Solve"}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Reply.Kind != tc.kind || result.Checkpoint.Phase != agentruntime.PhaseCompleted || result.Checkpoint.ContractHash != h.ContractHash() || result.Steps != 1 {
				t.Fatalf("result=%+v", result)
			}
			if tc.kind == agentsdk.AgentReplyOutput && result.Reply.Output.Answer != "42" {
				t.Fatal(result.Reply)
			}
			if len(result.Events) != 2 || len(result.Messages) != 3 || result.Checkpoint.Append != nil {
				t.Fatalf("diagnostics=%+v", result)
			}
		})
	}
}

func TestRunAgentGeneratedScopeAndTaskCatalog(t *testing.T) {
	e, h, o := runUnitEnv(t, runResponse("done", "complete", `{"kind":"output","output":{"answer":"ok"}}`))
	e.appID = uuid.NewString()
	o.Scope = wire.RuntimeContext{}
	first, err := RunAgent(t, t.Context(), e, h, runInput{}, o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunAgent(t, t.Context(), e, h, runInput{}, o)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scope.AgentID != e.appID || first.Scope.RunID == second.Scope.RunID || first.Scope.InvocationToken == second.Scope.InvocationToken || first.Scope.Caller.Kind != "application" || first.Scope.Caller.User != nil || first.Scope.Caller.Initiator != nil {
		t.Fatalf("first=%+v second=%+v", first.Scope, second.Scope)
	}
	encoded, err := json.Marshal(e.MockModel("reasoning").Requests())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"output(args:", "requestUpgrade(args:"} {
		if strings.Contains(string(encoded), name) {
			t.Fatalf("task exposes %s: %s", name, encoded)
		}
	}
	if !strings.Contains(string(encoded), "webSearch(args:") {
		t.Fatal("task omitted supported platform capabilities")
	}
}

type runNumbers struct {
	Signed   int64  `json:"signed"`
	Unsigned uint64 `json:"unsigned"`
}

func TestRunAgentExactIntegerContracts(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"maxima", `{"signed":9223372036854775807,"unsigned":18446744073709551615}`, true},
		{"min signed", `{"signed":-9223372036854775808,"unsigned":0}`, true},
		{"signed overflow", `{"signed":9223372036854775808,"unsigned":0}`, false},
		{"signed underflow", `{"signed":-9223372036854775809,"unsigned":0}`, false},
		{"unsigned overflow", `{"signed":0,"unsigned":18446744073709551616}`, false},
		{"unsigned underflow", `{"signed":0,"unsigned":-1}`, false},
		{"large fractional integer", `{"signed":9007199254740992.5,"unsigned":0}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := agentsdk.New(agentsdk.Config{Description: "Exact integer task"})
			a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Task model"})
			h := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runNumbers, runNumbers]{Slug: "numbers", Description: "Numbers", Instructions: "Complete exactly", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
			manifest := a.Manifest()
			for _, schema := range []json.RawMessage{manifest.AgentDefinitions[0].InputSchema, manifest.AgentDefinitions[0].OutputSchema} {
				err := agentruntime.ValidateJSON(schema, json.RawMessage(tc.output))
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%t err=%v value=%s", tc.valid, err, tc.output)
				}
			}
			models, err := prepareModels(t.Context(), manifest, Options{Mocks: map[string]testutil.MockConfig{"reasoning": {ID: "numbers", Responses: []testutil.MockResponse{runResponse("done", "complete", `{"kind":"output","output":`+tc.output+`}`)}}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			e, _, o := runUnitEnv(t)
			e.Agent = a
			e.models = models
			o.Scope = wire.RuntimeContext{}
			o.MaxSteps = 1
			input := runNumbers{Signed: math.MaxInt64, Unsigned: math.MaxUint64}
			r, err := RunAgent(t, t.Context(), e, h, input, o)
			if !tc.valid {
				if err == nil || !strings.Contains(err.Error(), "step budget") || r.Reply != nil {
					t.Fatalf("result=%+v err=%v", r, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want runNumbers
			if err := json.Unmarshal([]byte(tc.output), &want); err != nil {
				t.Fatal(err)
			}
			if *r.Reply.Output != want {
				t.Fatalf("output=%+v want=%+v", r.Reply.Output, want)
			}
			encoded, _ := json.Marshal(e.MockModel("reasoning").Requests())
			for _, number := range []string{"9223372036854775807", "18446744073709551615"} {
				if !strings.Contains(string(encoded), number) {
					t.Fatalf("input lost precision: %s", encoded)
				}
			}
		})
	}
}

func TestRunAgentCompactionChargesUsage(t *testing.T) {
	for _, limit := range []int64{100, 70} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			summary := testutil.MockResponse{Text: "Summary of the input.", Usage: stream.UsageFrom(20, 30)}
			large := testutil.MockResponse{Text: strings.Repeat("large context ", 4000), Usage: stream.UsageFrom(5, 5)}
			complete := runResponse("done", "complete", `{"kind":"output","output":{"answer":"ok"}}`)
			e, h, o := runUnitEnvBudget(t, &agentsdk.AgentBudget{Steps: 3, Tokens: limit}, session.ModelLimits{Input: 5000, Output: 1000}, large, summary, complete)
			r, err := RunAgent(t, t.Context(), e, h, runInput{Text: "Complete after gathering context"}, o)
			if r.Tokens != 75 || r.Steps != 3 {
				t.Fatalf("tokens=%d steps=%d err=%v", r.Tokens, r.Steps, err)
			}
			if limit == 70 {
				if err == nil || !strings.Contains(err.Error(), "token budget") || r.Reply != nil {
					t.Fatalf("result=%+v err=%v", r, err)
				}
			} else if err != nil || r.Reply == nil {
				t.Fatalf("result=%+v err=%v", r, err)
			}
			requests := e.MockModel("reasoning").Requests()
			if len(requests) != 3 || len(requests[0].Tools) == 0 || len(requests[1].Tools) != 0 || len(requests[2].Tools) == 0 {
				t.Fatalf("model requests=%d", len(requests))
			}
		})
	}
}

func TestRunAgentInvalidCompletionAndBudget(t *testing.T) {
	for _, input := range []string{`{"kind":"output","output":{}}`, `{"kind":"output","output":{"answer":42}}`, `{"kind":"output","output":{"answer":"ok","unknown":true}}`} {
		t.Run(input, func(t *testing.T) {
			e, h, opts := runUnitEnv(t, runResponse("invalid", "complete", input))
			opts.MaxSteps = 1
			result, err := RunAgent(t, t.Context(), e, h, runInput{}, opts)
			if err == nil || !strings.Contains(err.Error(), "step budget") || result.Reply != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			encoded, _ := json.Marshal(result.Messages)
			if !strings.Contains(string(encoded), "complete input") {
				t.Fatalf("missing validation error: %s", encoded)
			}
		})
	}
}

func TestRunAgentRejectsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*Env, *RunOptions)
	}{
		{"factory", "ExecutorFactory", func(_ *Env, o *RunOptions) { o.ExecutorFactory = nil }},
		{"contract", "contract mismatch", func(_ *Env, o *RunOptions) {
			o.Scope.Definition = &wire.RuntimeAgentDefinition{Slug: "task", ContractHash: strings.Repeat("0", 64)}
		}},
		{"caller", "application-owned", func(_ *Env, o *RunOptions) { o.Scope.Caller.Access = wire.AccessPublic }},
		{"token", "invocation token", func(_ *Env, o *RunOptions) { o.Scope.InvocationToken = "bad" }},
		{"id", "scope run", func(_ *Env, o *RunOptions) { o.Scope.RunID = "bad" }},
		{"app mismatch", "app identity mismatch", func(_ *Env, o *RunOptions) { o.Scope.AgentID = uuid.NewString() }},
		{"app malformed", "scope app ID", func(_ *Env, o *RunOptions) { o.Scope.AgentID = "bad" }},
		{"nil run", "scope run", func(_ *Env, o *RunOptions) { o.Scope.RunID = uuid.Nil.String() }},
		{"conversation", "scope run/conversation/bridge ID", func(_ *Env, o *RunOptions) { o.Scope.ConversationID = "bad" }},
		{"partial scope", "Scope requires", func(_ *Env, o *RunOptions) { o.Scope = wire.RuntimeContext{RunID: uuid.NewString()} }},
		{"user", "scope caller", func(_ *Env, o *RunOptions) { o.Scope.Caller.User = &wire.CallerUser{ID: "human"} }},
		{"initiator", "application-owned", func(_ *Env, o *RunOptions) { o.Scope.Caller.Initiator = &wire.CallerUser{ID: "human"} }},
		{"job", "application-owned", func(_ *Env, o *RunOptions) { o.Scope.Job = &wire.RuntimeJobContext{} }},
		{"model", "not registered", func(e *Env, _ *RunOptions) { e.models = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, h, o := runUnitEnv(t)
			tc.change(e, &o)
			_, err := RunAgent(t, t.Context(), e, h, runInput{}, o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	a := agentsdk.New(agentsdk.Config{Description: "Parent test"})
	a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Task model"})
	h := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "leaf", Description: "Leaf", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
	parent := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "parent", Description: "Parent", Instructions: "Delegate", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1, MaxSubagentCalls: 1, MaxConcurrentSubagents: 1, Subagents: []agentsdk.Subagent{h}})
	e, _, o := runUnitEnv(t)
	e.Agent = a
	if _, err := RunAgent(t, t.Context(), e, parent, runInput{}, o); err == nil || !strings.Contains(err.Error(), "leaf") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunAgentCanceled(t *testing.T) {
	e, h, o := runUnitEnv(t, runResponse("done", "complete", `{"kind":"output","output":{"answer":"ok"}}`))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := RunAgent(t, ctx, e, h, runInput{}, o)
	if !errors.Is(err, context.Canceled) || result.Reply != nil || result.Steps != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunAgentRequiresMCPDependencies(t *testing.T) {
	a := agentsdk.New(agentsdk.Config{Description: "MCP task test"})
	a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Task model"})
	mcp := a.RegisterMCP(&agentsdk.MCP{Slug: "external", Name: "External", URL: "https://example.com/mcp", AuthMode: agentsdk.MCPAuthNone, BindingMode: agentsdk.BindingShared})
	h := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "task", Description: "Task", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1, MCPs: []*agentsdk.MCPHandle{mcp}})
	e, _, o := runUnitEnv(t)
	e.Agent = a
	_, err := RunAgent(t, t.Context(), e, h, runInput{}, o)
	if err == nil || !strings.Contains(err.Error(), `MCP "external" requires explicit discovery`) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegisteredRunBackendRequiresServices(t *testing.T) {
	b := &registeredRunBackend{private: map[string]bool{}}
	for _, id := range []string{"air//file_write", "air//web_search"} {
		_, err := b.Invoke(t.Context(), chatruntime.Invocation{CapabilityID: id, Input: json.RawMessage(`{}`)})
		if err == nil || !strings.Contains(err.Error(), "unsupported service") {
			t.Fatal(err)
		}
	}
	if len(b.calls) != 2 {
		t.Fatal(b.calls)
	}
}

func TestRunAgentDenoPrivateArtifact(t *testing.T) {
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
	factory, err := Executor(ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/run-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "db", "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	artifact := filepath.Join(t.TempDir(), "report.txt")
	cancelEntered := make(chan struct{}, 1)
	var h *agentsdk.AgentHandle[runInput, runOutput]
	var budgetHandle *agentsdk.AgentHandle[runInput, runOutput]
	globalCalls := 0
	env := New(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "Deno task"})
		a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Task model"})
		a.RegisterTool(tool.New("artifact").Description("Global collision").SchemaFromStruct(runInput{}).Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
			globalCalls++
			return tool.Result{Output: "global"}, nil
		}).Build(), agentsdk.AccessAdmin)
		private := tool.New("artifact").Description("Write test artifact").SchemaFromStruct(runInput{}).OutputSchemaFromStruct(runOutput{}).Execute(func(ctx context.Context, raw json.RawMessage, call tool.CallOptions) (tool.Result, error) {
			caller := agentsdk.CallerFromContext(ctx)
			if caller.Kind() != agentsdk.CallerApplication || caller.Access() != agentsdk.AccessAdmin {
				return tool.Result{}, errors.New("wrong caller")
			}
			if _, ok := caller.User(); ok {
				return tool.Result{}, errors.New("unexpected human")
			}
			if call.ToolCallID != "script" {
				return tool.Result{}, errors.New("lost call ID")
			}
			var args runInput
			if err := json.Unmarshal(raw, &args); err != nil {
				return tool.Result{}, err
			}
			if args.Text == "fail" {
				return tool.Result{}, errors.New("artifact deliberately failed")
			}
			if args.Text == "cancel" {
				cancelEntered <- struct{}{}
				<-ctx.Done()
				return tool.Result{}, ctx.Err()
			}
			if err := os.WriteFile(artifact, []byte(args.Text), 0600); err != nil {
				return tool.Result{}, err
			}
			a.Logger(ctx).Info("artifact written")
			return tool.Result{Output: `{"answer":"written"}`, Title: "Artifact"}, nil
		}).Build()
		h = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "task", Description: "Artifact task", Instructions: "Write an artifact and complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1, Tools: []tool.Tool{private}})
		budgetHandle = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[runInput, runOutput]{Slug: "budget_task", Description: "Budget task", Instructions: "Write an artifact and complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1, Budget: &agentsdk.AgentBudget{Steps: 3, Tokens: 15}, Tools: []tool.Tool{private}})
		return a
	})
	configure := func(responses ...testutil.MockResponse) {
		t.Helper()
		m := env.MockModel("reasoning")
		if err := m.Configure(testutil.MockConfig{ID: m.ID(), Responses: responses}); err != nil {
			t.Fatal(err)
		}
	}
	configure(runResponse("script", "run_js", `{"code":"return await tools.artifact({text: 'actual Deno artifact'});","description":"Write report"}`), runResponse("done", "complete", `{"kind":"output","output":{"answer":"written"}}`))
	result, err := RunAgent(t, ctx, env, h, runInput{Text: "Write report"}, RunOptions{ExecutorFactory: factory, MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Scope.AgentID != env.appID || result.Scope.AgentID != uuid.Nil.String() {
		t.Fatalf("generated scope=%+v", result.Scope)
	}
	content, err := os.ReadFile(artifact)
	if err != nil || string(content) != "actual Deno artifact" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if globalCalls != 0 || len(result.AppCalls) != 1 || result.AppCalls[0].Title != "Artifact" || len(result.AppCalls[0].Logs) != 1 || result.Reply.Output.Answer != "written" {
		t.Fatalf("result=%+v global=%d", result, globalCalls)
	}
	if len(result.Calls) != 1 || result.Calls[0].CapabilityID != capability.Local(capability.Tool, "", "artifact").ID() || result.Scope.Definition.ContractHash != h.ContractHash() {
		t.Fatalf("calls=%+v scope=%+v", result.Calls, result.Scope)
	}
	requests, _ := json.Marshal(env.MockModel("reasoning").Requests())
	if !strings.Contains(string(requests), "written") {
		t.Fatalf("private result missing from model context: %s", requests)
	}
	for _, req := range env.Airlock.Requests() {
		if strings.HasPrefix(req.Path, "/api/agent/run") {
			t.Fatalf("created host lifecycle: %+v", req)
		}
	}
	t.Run("dispatch boundaries", func(t *testing.T) {
		catalog, err := capability.DefinitionCatalog(env.Agent.Manifest(), *result.Scope.Definition, capability.Discovery{})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, want string
			mutate     func(*wire.RuntimeContext)
			id         string
		}{
			{"hash", "HTTP 409", func(s *wire.RuntimeContext) {
				s.Definition = &wire.RuntimeAgentDefinition{Slug: h.Slug(), ContractHash: strings.Repeat("0", 64)}
			}, "tool//artifact"},
			{"app ownership", "HTTP 403", func(s *wire.RuntimeContext) { s.AgentID = uuid.NewString() }, "tool//artifact"},
			{"unknown private", "unknown test capability", func(*wire.RuntimeContext) {}, "tool//unregistered"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := result.Scope
				tc.mutate(&s)
				backend := &chatBackend{handler: env.Agent.Handler(), scope: s, catalog: catalog}
				_, err := backend.Invoke(t.Context(), chatruntime.Invocation{CapabilityID: tc.id, ToolCallID: "boundary", Input: json.RawMessage(`{"text":"should not write"}`)})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v", err)
				}
			})
		}
	})
	t.Run("native error", func(t *testing.T) {
		configure(runResponse("script", "run_js", `{"code":"return await tools.artifact({text: 'fail'});","description":"Attempt artifact"}`), runResponse("question", "complete", `{"kind":"needs_input","question":"Artifact failed; retry?"}`))
		r, err := RunAgent(t, ctx, env, h, runInput{}, RunOptions{Scope: runScope(), ExecutorFactory: factory})
		if err != nil {
			t.Fatal(err)
		}
		if r.Reply.Kind != agentsdk.AgentReplyNeedsInput || len(r.AppCalls) != 1 || !strings.Contains(r.AppCalls[0].Error, "artifact deliberately failed") {
			t.Fatalf("result=%+v", r)
		}
		encoded, _ := json.Marshal(r.Messages)
		if !strings.Contains(string(encoded), "side effects may have occurred") {
			t.Fatalf("missing runtime error semantics: %s", encoded)
		}
	})
	t.Run("task capability exclusions", func(t *testing.T) {
		for _, code := range []string{`return await air.output({path: 'report.txt'});`, `return await air.requestUpgrade({});`} {
			configure(runResponse("forbidden", "run_js", `{"code":`+strconv.Quote(code)+`,"description":"Attempt unsupported service"}`), runResponse("question", "complete", `{"kind":"needs_input","question":"Service unavailable"}`))
			calls := 0
			backend := runBackendFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
				calls++
				return tool.Result{}, errors.New("forbidden backend call")
			})
			r, err := RunAgent(t, ctx, env, h, runInput{}, RunOptions{ExecutorFactory: factory, Backend: backend})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 || len(r.Calls) != 0 || r.Reply.Kind != agentsdk.AgentReplyNeedsInput {
				t.Fatalf("calls=%d result=%+v", calls, r)
			}
		}
	})
	t.Run("tokens prevent private effects", func(t *testing.T) {
		configure(runResponse("script", "run_js", `{"code":"return await tools.artifact({text: 'over budget'});","description":"Write over budget"}`))
		r, err := RunAgent(t, ctx, env, budgetHandle, runInput{}, RunOptions{ExecutorFactory: factory})
		if err == nil || !strings.Contains(err.Error(), "token budget") || r.Tokens != 15 || len(r.Calls) != 0 || r.Reply != nil {
			t.Fatalf("result=%+v err=%v", r, err)
		}
		content, err := os.ReadFile(artifact)
		if err != nil || string(content) != "actual Deno artifact" {
			t.Fatalf("budget allowed effects: %q err=%v", content, err)
		}
	})
	// Cancellation stops the runtime and tears down the Deno realm.
	configure(runResponse("script", "run_js", `{"code":"return await tools.artifact({text: 'cancel'});","description":"Wait for cancellation"}`))
	short, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-cancelEntered:
			cancel()
		case <-short.Done():
		}
	}()
	result, err = RunAgent(t, short, env, h, runInput{}, RunOptions{Scope: runScope(), ExecutorFactory: factory, Timeout: 5 * time.Second})
	if !errors.Is(err, context.Canceled) || result.Reply != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type runBackendFunc func(context.Context, chatruntime.Invocation) (tool.Result, error)

func (f runBackendFunc) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	return f(ctx, in)
}
