package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
)

var errCrash = errors.New("simulated interrupted attempt")

type fatalError struct{ error }

func (fatalError) FatalToolError() bool { return true }

// Every persistence boundary JSON-roundtrips to expose accidental heap state.
type memoryStore struct {
	cp          json.RawMessage
	messages    []session.Message
	saves       []*Checkpoint
	fail        func(*Checkpoint) bool
	failAfter   bool
	compactions int
	loadErr     error
}

func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
func (s *memoryStore) Load(context.Context) ([]session.Message, error) {
	return clone(s.messages), s.loadErr
}
func (*memoryStore) Append(context.Context, []session.Message) error {
	panic("non-atomic Append called")
}
func (s *memoryStore) Compact(_ context.Context, msgs []session.Message, _ int) error {
	s.compactions++
	s.messages = clone(msgs)
	return nil
}
func (s *memoryStore) LoadCheckpoint(context.Context) (*Checkpoint, error) {
	if s.cp == nil {
		return nil, nil
	}
	var cp Checkpoint
	if err := json.Unmarshal(s.cp, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}
func (s *memoryStore) SaveCheckpoint(ctx context.Context, cp *Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	previous, err := s.LoadCheckpoint(ctx)
	if err != nil {
		return err
	}
	if previous != nil && previous.Revision == cp.Revision && string(raw) == string(s.cp) {
		return nil
	}
	expected := int64(1)
	if previous != nil {
		expected = previous.Revision + 1
	}
	if cp.Revision != expected {
		return errors.New("stale checkpoint revision")
	}
	fail := s.fail != nil && s.fail(cp)
	if fail {
		s.fail = nil
	}
	if fail && !s.failAfter {
		return errCrash
	}
	s.cp = append(json.RawMessage(nil), raw...)
	s.messages = append(s.messages, clone(cp.Append)...)
	s.saves = append(s.saves, clone(cp))
	if fail {
		return errCrash
	}
	return nil
}

type controller struct {
	steps         int
	limit         int
	log           []string
	handles       map[string]wire.AgentRunInfo
	spawnCalls    int
	continueCalls int
	waitCalls     int
	waiting       bool
	waitRequests  []wire.AgentWaitRequest
	waitResult    *wire.AgentWaitResult
	completeErr   error
	beforeErr     error
	completeCalls int
	check         func(string)
}

func (c *controller) record(s string) {
	c.log = append(c.log, s)
	if c.check != nil {
		c.check(s)
	}
}
func (c *controller) BeforeModel(context.Context) error {
	if c.beforeErr != nil {
		return c.beforeErr
	}
	if c.limit > 0 && c.steps >= c.limit {
		return errors.New("step budget exceeded")
	}
	c.steps++
	c.record("model")
	return nil
}
func (c *controller) Spawn(_ context.Context, id, slug string, _ json.RawMessage) (wire.AgentRunInfo, error) {
	c.spawnCalls++
	c.record("spawn:" + id)
	if c.handles == nil {
		c.handles = map[string]wire.AgentRunInfo{}
	}
	if info, ok := c.handles[id]; ok {
		return info, nil
	}
	info := wire.AgentRunInfo{ID: "child-" + id, SessionID: "session-" + id, Definition: slug, Status: "running"}
	c.handles[id] = info
	return info, nil
}
func (c *controller) Continue(ctx context.Context, id, sessionID, _ string) (wire.AgentRunInfo, error) {
	c.continueCalls++
	c.record("continue:" + id)
	if c.handles == nil {
		c.handles = map[string]wire.AgentRunInfo{}
	}
	if info, ok := c.handles[id]; ok {
		return info, nil
	}
	info := wire.AgentRunInfo{ID: "continued-" + id, SessionID: sessionID, Status: "running"}
	c.handles[id] = info
	return info, nil
}
func (c *controller) Get(_ context.Context, ids []string) ([]wire.AgentRunInfo, error) {
	c.record("get")
	return []wire.AgentRunInfo{{ID: ids[0], Status: "completed"}}, nil
}
func (c *controller) Wait(_ context.Context, id string, req wire.AgentWaitRequest) (wire.AgentWaitResult, error) {
	c.record("wait:" + id)
	c.waitCalls++
	c.waitRequests = append(c.waitRequests, clone(req))
	if c.waiting {
		return wire.AgentWaitResult{}, ErrWaiting
	}
	if c.waitResult != nil {
		return clone(*c.waitResult), nil
	}
	return wire.AgentWaitResult{Reason: "ready", Completed: []wire.AgentRunInfo{{ID: req.IDs[0], Status: "completed"}}}, nil
}
func (c *controller) Cancel(_ context.Context, ids []string) (wire.AgentCancelResult, error) {
	c.record("cancel")
	return wire.AgentCancelResult{Calls: []wire.AgentRunInfo{{ID: ids[0], Status: "cancelled"}}}, nil
}
func (c *controller) Complete(_ context.Context, _ wire.AgentReply) error {
	c.record("complete")
	c.completeCalls++
	return c.completeErr
}

type sink struct {
	texts       []string
	calls       []stream.ToolCallEvent
	results     []stream.ToolResultEvent
	compactions []bus.AutomaticCompactionFinishedPayload
}

func (s *sink) OnTextDelta(e stream.TextDeltaEvent)                              { s.texts = append(s.texts, e.Text) }
func (s *sink) OnToolCall(e stream.ToolCallEvent)                                { s.calls = append(s.calls, e) }
func (s *sink) OnToolResult(e stream.ToolResultEvent)                            { s.results = append(s.results, e) }
func (*sink) OnPermissionAsked(bus.PermissionAskedPayload)                       { panic("unexpected permission") }
func (*sink) OnAutomaticCompactionStarted(bus.AutomaticCompactionStartedPayload) {}
func (s *sink) OnAutomaticCompactionFinished(e bus.AutomaticCompactionFinishedPayload) {
	s.compactions = append(s.compactions, e)
}
func (*sink) OnSuspension(*sol.SuspensionContext) { panic("unexpected suspension") }

type backendFunc func(context.Context, chatruntime.Invocation) (tool.Result, error)

func (f backendFunc) Invoke(ctx context.Context, i chatruntime.Invocation) (tool.Result, error) {
	return f(ctx, i)
}

type executor struct {
	calls, closes int
	execute       func(context.Context, string, jsexec.Invoker) (jsexec.Result, error)
	closeErr      error
}

func (e *executor) Execute(ctx context.Context, code string, invoker jsexec.Invoker) (jsexec.Result, error) {
	e.calls++
	if e.execute != nil {
		return e.execute(ctx, code, invoker)
	}
	return jsexec.Result{Output: json.RawMessage(`42`)}, nil
}
func (e *executor) Close() error { e.closes++; return e.closeErr }

func input(responses ...[]stream.Event) (Input, *testutil.MockLanguageModel) {
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: responses})
	return Input{
		Definition: wire.AgentDefinition{Slug: "task", ContractHash: "contract", Instructions: "Do the task.", OutputSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}`), Budget: wire.AgentBudget{Steps: 150}},
		Message:    "start", Model: model, ModelLimits: session.ModelLimits{Input: 80000, Output: 4096},
		Store: &memoryStore{}, Controller: &controller{}, Sink: &sink{}, Redactor: func(s string) string { return s },
		Backend: backendFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
			return tool.Result{}, errors.New("unexpected backend call")
		}),
		ExecutorFactory: func(context.Context, []capability.Definition) (jsexec.Session, error) {
			return nil, errors.New("unexpected executor allocation")
		},
	}, model
}

func call(id, name, args string) stream.Event {
	return stream.Event{Type: stream.EventToolCall, Data: stream.ToolCallEvent{ToolCallID: id, ToolName: name, Input: json.RawMessage(args)}}
}
func batch(calls ...stream.Event) []stream.Event {
	return append(calls, stream.Event{Type: stream.EventFinish, Data: stream.FinishEvent{FinishReason: stream.FinishReasonToolCalls, Usage: testutil.MockUsage(10, 10)}})
}
func complete(id string) stream.Event {
	return call(id, "complete", `{"kind":"output","output":{"answer":42}}`)
}
func js(id string) stream.Event {
	return call(id, "run_js", `{"code":"return 42;","description":"Calculate"}`)
}
func addChild(in *Input) {
	in.Definition.Subagents = []string{"research"}
	in.Subagents = []wire.AgentDefinition{{Slug: "research", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}}
}

func TestRunCompletionAndOrderedTail(t *testing.T) {
	for _, kind := range []string{"output", "needs_input"} {
		t.Run(kind, func(t *testing.T) {
			finish := complete("done")
			if kind == "needs_input" {
				finish = call("done", "complete", `{"kind":"needs_input","question":"Which account?"}`)
			}
			in, model := input(batch(call("inspect", "get_calls", `{"ids":["child"]}`), finish, js("never")))
			result, err := Run(t.Context(), in)
			if err != nil || result.Reply.Kind != kind || result.Waiting {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			c, store := in.Controller.(*controller), in.Store.(*memoryStore)
			if !reflect.DeepEqual(c.log, []string{"model", "get", "complete"}) {
				t.Fatal(c.log)
			}
			cp, _ := store.LoadCheckpoint(t.Context())
			if cp.Phase != PhaseCompleted || cp.Next != 3 || len(cp.Response) == 0 || cp.Calls[2].Result.Parts[0].Tool.Outcome != "denied" {
				t.Fatalf("checkpoint=%+v", cp)
			}
			for _, msg := range store.messages {
				if msg.ID == "" {
					t.Fatal("message lacks durable ID")
				}
			}
			count := len(store.messages)
			result, err = Run(t.Context(), in)
			if err != nil || result.Reply.Kind != kind || len(model.DoStreamCalls) != 1 || len(store.messages) != count || c.completeCalls != 1 {
				t.Fatalf("completed replay=%+v %v", result, err)
			}
		})
	}
}

func TestRunCleanupFailurePreservesExecutionOutcome(t *testing.T) {
	cleanupErr := errors.New("executor teardown failed")
	for _, tc := range []struct {
		name       string
		last       stream.Event
		failCommit bool
		commitAck  bool
		waiting    bool
		failure    error
		completed  bool
	}{
		{name: "completed output", last: complete("done"), completed: true},
		{name: "completed needs input", last: call("done", "complete", `{"kind":"needs_input","question":"Which account?"}`), completed: true},
		{name: "waiting", last: call("wait", "wait_calls", `{"ids":["child"],"mode":"all"}`), waiting: true},
		{name: "execution failure", last: complete("done"), failure: fatalError{errCrash}},
		{name: "completion commit rejected", last: complete("done"), failCommit: true},
		{name: "completion acknowledgement lost", last: complete("done"), failCommit: true, commitAck: true, completed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(js("effect"), tc.last, js("never")))
			store, control := in.Store.(*memoryStore), in.Controller.(*controller)
			control.waiting, control.completeErr = tc.waiting, tc.failure
			if tc.failCommit {
				store.fail = func(cp *Checkpoint) bool { return cp.Phase == PhaseCompleted }
				store.failAfter = tc.commitAck
			}
			e := &executor{closeErr: cleanupErr}
			in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
			result, err := Run(t.Context(), in)
			if e.calls != 1 || e.closes != 1 || len(model.DoStreamCalls) != 1 {
				t.Fatalf("executor=%+v model calls=%d", e, len(model.DoStreamCalls))
			}
			cp, loadErr := store.LoadCheckpoint(t.Context())
			if loadErr != nil || (cp.Phase == PhaseCompleted) != tc.completed {
				t.Fatalf("checkpoint=%+v error=%v", cp, loadErr)
			}
			switch {
			case tc.completed && !tc.failCommit:
				if err != nil || result == nil || !reflect.DeepEqual(result.Reply, cp.Reply) || result.Waiting || !errors.Is(result.CleanupError, cleanupErr) {
					t.Fatalf("completed result=%+v error=%v", result, err)
				}
			case tc.waiting:
				if result == nil || !result.Waiting || result.Reply != nil || !errors.Is(result.CleanupError, cleanupErr) || !errors.Is(err, cleanupErr) || !errors.Is(err, ErrWaiting) {
					t.Fatalf("waiting result=%+v error=%v", result, err)
				}
			default:
				wantErr := tc.failure
				if wantErr == nil {
					wantErr = errCrash
				}
				if result != nil || !errors.Is(err, cleanupErr) || !errors.Is(err, wantErr) {
					t.Fatalf("unfinished result=%+v error=%v", result, err)
				}
			}
			if tc.completed {
				resumed, err := Run(t.Context(), in)
				if err != nil || resumed == nil || !reflect.DeepEqual(resumed.Reply, cp.Reply) || resumed.CleanupError != nil || e.calls != 1 || e.closes != 1 || len(model.DoStreamCalls) != 1 {
					t.Fatalf("completed replay=%+v error=%v executor=%+v", resumed, err, e)
				}
			}
		})
	}
}

func TestRunInterruptedModelReservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		afterCharge bool
		limit       int
	}{
		{name: "checkpoint before reservation", limit: 1},
		{name: "charged before request", afterCharge: true, limit: 2},
		{name: "charged last step before request", afterCharge: true, limit: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(complete("done")))
			store, control := in.Store.(*memoryStore), in.Controller.(*controller)
			control.limit = tc.limit
			in.Definition.Budget.Steps = int64(tc.limit)
			if tc.afterCharge {
				control.check = func(name string) {
					if name == "model" {
						panic(errCrash)
					}
				}
			} else {
				store.fail = func(cp *Checkpoint) bool { return cp.Phase == PhaseModel }
				store.failAfter = true
			}
			firstErr := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						if r != errCrash {
							panic(r)
						}
						err = errCrash
					}
				}()
				_, err = Run(t.Context(), in)
				return err
			}()
			if !errors.Is(firstErr, errCrash) || len(model.DoStreamCalls) != 0 {
				t.Fatalf("interruption=%v model calls=%d", firstErr, len(model.DoStreamCalls))
			}
			cp, _ := store.LoadCheckpoint(t.Context())
			if cp.Phase != PhaseModel || (control.steps == 1) != tc.afterCharge {
				t.Fatalf("checkpoint=%+v steps=%d", cp, control.steps)
			}
			control.check = nil
			result, err := Run(t.Context(), in)
			if tc.afterCharge && tc.limit == 1 {
				if err == nil || !strings.Contains(err.Error(), "step budget exceeded") || result != nil || control.steps != 1 || len(model.DoStreamCalls) != 0 {
					t.Fatalf("exhausted recovery=%+v error=%v steps=%d requests=%d", result, err, control.steps, len(model.DoStreamCalls))
				}
			} else if err != nil || result == nil || result.Reply == nil || control.steps != tc.limit || len(model.DoStreamCalls) != 1 {
				t.Fatalf("recovery=%+v error=%v steps=%d requests=%d", result, err, control.steps, len(model.DoStreamCalls))
			}
		})
	}
}

func TestRunWaitResumesExactCallAndTail(t *testing.T) {
	in, model := input(batch(js("before"), call("child", "spawn_research", `{"query":"find"}`), call("park", "wait_calls", `{"ids":["child-child"],"mode":"all","timeoutMs":1234}`), js("after"), complete("done")))
	addChild(&in)
	c, store := in.Controller.(*controller), in.Store.(*memoryStore)
	c.waiting = true
	var realms []*executor
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) {
		e := &executor{}
		realms = append(realms, e)
		return e, nil
	}
	c.check = func(name string) {
		if name == "model" {
			return
		}
		cp, _ := store.LoadCheckpoint(t.Context())
		if cp.Phase != PhaseDispatch || cp.Calls[cp.Next].Result != nil {
			t.Fatalf("dispatch without checkpoint: %+v", cp)
		}
	}
	for i := 0; i < 2; i++ {
		result, err := Run(t.Context(), in)
		if !errors.Is(err, ErrWaiting) || !result.Waiting || result.Reply != nil {
			t.Fatalf("park=%+v %v", result, err)
		}
		cp, _ := store.LoadCheckpoint(t.Context())
		if cp.Phase != PhaseWaiting || cp.Next != 2 || cp.Calls[2].Result != nil {
			t.Fatalf("park checkpoint=%+v", cp)
		}
	}
	c.waiting = false
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil {
		t.Fatalf("resume=%+v %v", result, err)
	}
	if len(model.DoStreamCalls) != 1 || c.steps != 1 || c.spawnCalls != 1 || c.waitCalls != 3 || len(realms) != 2 {
		t.Fatalf("model=%d controller=%+v realms=%d", len(model.DoStreamCalls), c, len(realms))
	}
	for _, e := range realms {
		if e.calls != 1 || e.closes != 1 {
			t.Fatalf("realm=%+v", e)
		}
	}
	for _, req := range c.waitRequests {
		if !reflect.DeepEqual(req, c.waitRequests[0]) {
			t.Fatal("resumed wait arguments changed")
		}
	}
	counts := map[string]int{}
	for _, msg := range store.messages {
		if msg.Role == "tool" {
			counts[msg.Parts[0].Tool.CallID]++
		}
	}
	for _, id := range []string{"before", "child", "park", "after", "done"} {
		if counts[id] != 1 {
			t.Fatalf("result counts=%v", counts)
		}
	}
}

func TestRunCrashBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   Phase
		next    int
		after   bool
		wantJS  int
		unknown bool
	}{
		{"captured before dispatch", PhaseTools, 0, true, 1, false},
		{"predispatch uncommitted", PhaseDispatch, 0, false, 1, false},
		{"predispatch committed", PhaseDispatch, 0, true, 0, true},
		{"postresult uncommitted", PhaseTools, 1, false, 1, true},
		{"postresult committed", PhaseTools, 1, true, 1, false},
		{"completion uncommitted", PhaseCompleted, 2, false, 1, false},
		{"completion committed", PhaseCompleted, 2, true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(js("effect"), complete("done")))
			store, e := in.Store.(*memoryStore), &executor{}
			store.failAfter = tc.after
			store.fail = func(cp *Checkpoint) bool { return cp.Phase == tc.phase && cp.Next == tc.next }
			in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
			if _, err := Run(t.Context(), in); !errors.Is(err, errCrash) {
				t.Fatalf("first error=%v", err)
			}
			result, err := Run(t.Context(), in)
			if err != nil || result.Reply == nil || e.calls != tc.wantJS || len(model.DoStreamCalls) != 1 {
				t.Fatalf("resume=%+v %v JS=%d model=%d", result, err, e.calls, len(model.DoStreamCalls))
			}
			var results []session.ToolPart
			for _, msg := range store.messages {
				if msg.Role == "tool" && msg.Parts[0].Tool.CallID == "effect" {
					results = append(results, *msg.Parts[0].Tool)
				}
			}
			if len(results) != 1 || strings.Contains(results[0].Output, "unknown outcome") != tc.unknown {
				t.Fatalf("results=%+v", results)
			}
		})
	}
}

func TestRunWaitCommitRecoveryAndTimeout(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted park", true: "committed park"}[after], func(t *testing.T) {
			in, model := input(batch(call("wait", "wait_calls", `{"ids":["child"],"mode":"any","timeoutMs":0}`), complete("done")))
			c, store := in.Controller.(*controller), in.Store.(*memoryStore)
			c.waiting = true
			store.failAfter = after
			store.fail = func(cp *Checkpoint) bool { return cp.Phase == PhaseWaiting }
			if _, err := Run(t.Context(), in); !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			c.waiting = false
			c.waitResult = &wire.AgentWaitResult{Reason: "timeout", Pending: []string{"child"}}
			result, err := Run(t.Context(), in)
			if err != nil || result.Reply == nil || c.waitCalls != 2 || len(model.DoStreamCalls) != 1 {
				t.Fatalf("result=%+v error=%v controller=%+v", result, err, c)
			}
			if !reflect.DeepEqual(c.waitRequests[0], c.waitRequests[1]) || c.waitRequests[1].TimeoutMS == nil || *c.waitRequests[1].TimeoutMS != 0 {
				t.Fatal(c.waitRequests)
			}
			cp, _ := store.LoadCheckpoint(t.Context())
			var wait wire.AgentWaitResult
			if err := json.Unmarshal([]byte(cp.Calls[0].Result.Parts[0].Tool.Output), &wait); err != nil || wait.Reason != "timeout" || len(wait.Pending) != 1 {
				t.Fatalf("wait=%+v error=%v", wait, err)
			}
			for _, entry := range c.log {
				if entry == "cancel" {
					t.Fatal("timeout cancelled children")
				}
			}
		})
	}
}

func TestRunModelStreamErrorNeverDispatchesPartialBatch(t *testing.T) {
	in, model := input([]stream.Event{js("partial"), {Type: stream.EventError, Data: stream.ErrorEvent{Error: errCrash}}}, batch(complete("done")))
	allocations := 0
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) {
		allocations++
		return &executor{}, nil
	}
	if _, err := Run(t.Context(), in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if allocations != 0 {
		t.Fatal("partial model response dispatched")
	}
	cp, _ := in.Store.LoadCheckpoint(t.Context())
	if cp.Phase != PhaseModel || len(cp.Calls) != 0 {
		t.Fatalf("checkpoint=%+v", cp)
	}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || allocations != 0 || len(model.DoStreamCalls) != 2 {
		t.Fatalf("result=%+v error=%v allocations=%d", result, err, allocations)
	}
}

func TestRunDeterministicAdmissionRecovery(t *testing.T) {
	for _, name := range []string{"spawn_research", "continue_agent"} {
		t.Run(name, func(t *testing.T) {
			args := `{"query":"find"}`
			if name == "continue_agent" {
				args = `{"sessionId":"existing","prompt":"follow up"}`
			}
			in, model := input(batch(call("admit", name, args), complete("done")))
			addChild(&in)
			store, c := in.Store.(*memoryStore), in.Controller.(*controller)
			store.fail = func(cp *Checkpoint) bool { return cp.Phase == PhaseTools && cp.Next == 1 }
			if _, err := Run(t.Context(), in); !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			if len(c.handles) != 1 {
				t.Fatalf("admission not durable: %+v", c)
			}
			result, err := Run(t.Context(), in)
			if err != nil || result.Reply == nil || len(c.handles) != 1 || c.spawnCalls+c.continueCalls != 2 || len(model.DoStreamCalls) != 1 {
				t.Fatalf("resume=%+v %v c=%+v", result, err, c)
			}
		})
	}
}

func TestRunKnownToolErrorAndCompletionRejection(t *testing.T) {
	in, model := input(batch(call("early", "complete", `{"kind":"output","output":{"answer":"bad"}}`), call("active", "complete", `{"kind":"output","output":{"answer":42}}`), call("cancel", "cancel_calls", `{"ids":["child"]}`), complete("done")))
	c := in.Controller.(*controller)
	c.completeErr = errors.New("active children remain")
	c.check = func(name string) {
		if name == "cancel" {
			c.completeErr = nil
		}
	}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || c.completeCalls != 2 || len(model.DoStreamCalls) != 1 {
		t.Fatalf("result=%+v %v c=%+v", result, err, c)
	}
	store := in.Store.(*memoryStore)
	cp, _ := store.LoadCheckpoint(t.Context())
	for _, index := range []int{0, 1} {
		if cp.Calls[index].Result.Parts[0].Tool.Outcome != "error" {
			t.Fatalf("result=%+v", cp.Calls[index])
		}
	}
}

func TestRunUnlimitedStepsAndBudgetGate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    wire.AgentBudget
		limit     int
		want      int
		completes bool
	}{
		{"timeout only exceeds fifty", wire.AgentBudget{TimeoutMS: 60000}, 0, 56, true},
		{"tokens only exceeds fifty", wire.AgentBudget{Tokens: 100000}, 0, 56, true},
		{"host step gate", wire.AgentBudget{Steps: 3}, 3, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var responses [][]stream.Event
			for i := 0; i < 55; i++ {
				responses = append(responses, batch(call(fmt.Sprintf("get-%d", i), "get_calls", `{"ids":["child"]}`)))
			}
			responses = append(responses, batch(complete("done")))
			in, model := input(responses...)
			in.Definition.Budget = tc.budget
			in.Controller.(*controller).limit = tc.limit
			result, err := Run(t.Context(), in)
			if (err == nil) != tc.completes || len(model.DoStreamCalls) != tc.want {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, len(model.DoStreamCalls))
			}
		})
	}
}

func TestRunInterruptedModelAndFreshContinuation(t *testing.T) {
	in, model := input(batch(js("discarded")), batch(complete("done")))
	store := in.Store.(*memoryStore)
	store.fail = func(cp *Checkpoint) bool { return cp.Phase == PhaseTools }
	if _, err := Run(t.Context(), in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || len(model.DoStreamCalls) != 2 {
		t.Fatalf("resume=%+v %v", result, err)
	}
	raw, _ := json.Marshal(model.DoStreamCalls[1])
	if !strings.Contains(string(raw), "No local tools") || strings.Contains(string(raw), "discarded") {
		t.Fatalf("recovery request=%s", raw)
	}
	store.cp = nil // Host creates a new run in the same completed session.
	in.Message = "follow up"
	in.Model = testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{StreamResponses: [][]stream.Event{batch(complete("continued"))}})
	result, err = Run(t.Context(), in)
	if err != nil || result.Reply == nil {
		t.Fatalf("continuation=%+v %v", result, err)
	}
	userMessages := []string{}
	for _, msg := range store.messages {
		if msg.Role == "user" {
			userMessages = append(userMessages, msg.Content)
		}
	}
	if userMessages[len(userMessages)-1] != "follow up" || userMessages[0] != "start" {
		t.Fatal(userMessages)
	}
}

func TestRunJavaScriptControlIsolation(t *testing.T) {
	in, _ := input(batch(js("script"), complete("done")))
	def := capability.Definition{Path: capability.Local(capability.Tool, "", "lookup"), Target: capability.App, InputSchema: json.RawMessage(`{"type":"object"}`)}
	in.Capabilities = []capability.Definition{def}
	backendCalls := 0
	in.Backend = backendFunc(func(_ context.Context, invocation chatruntime.Invocation) (tool.Result, error) {
		backendCalls++
		if invocation.ToolCallID != "script" || invocation.CapabilityID != def.Path.ID() {
			t.Fatal(invocation)
		}
		return tool.Result{Output: `{"ok":true}`, Attachments: []tool.Attachment{{Data: "s3ref:/image.png", MimeType: "image/png"}}}, nil
	})
	e := &executor{execute: func(ctx context.Context, _ string, inv jsexec.Invoker) (jsexec.Result, error) {
		for _, control := range []string{"complete", "spawn_research", "continue_agent", "get_calls", "wait_calls", "cancel_calls"} {
			if _, err := inv.Invoke(ctx, control, json.RawMessage(`[{}]`)); err == nil {
				t.Errorf("control %s exposed to JS", control)
			}
		}
		out, err := inv.Invoke(ctx, def.Path.ID(), json.RawMessage(`[{}]`))
		return jsexec.Result{Output: out}, err
	}}
	in.ExecutorFactory = func(_ context.Context, defs []capability.Definition) (jsexec.Session, error) {
		if !reflect.DeepEqual(defs, in.Capabilities) {
			t.Fatalf("bindings=%+v", defs)
		}
		return e, nil
	}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || backendCalls != 1 || e.closes != 1 {
		t.Fatalf("result=%+v %v e=%+v", result, err, e)
	}
	cp, _ := in.Store.LoadCheckpoint(t.Context())
	if len(cp.Calls[0].Result.Parts) != 2 || cp.Calls[0].Result.Parts[1].File.Source != "/image.png" {
		t.Fatalf("attachment=%+v", cp.Calls[0].Result)
	}
}

func TestRunCancellationLeavesUnknownDispatch(t *testing.T) {
	in, _ := input(batch(js("effect"), complete("done")))
	ctx, cancel := context.WithCancel(t.Context())
	e := &executor{execute: func(context.Context, string, jsexec.Invoker) (jsexec.Result, error) {
		cancel()
		return jsexec.Result{}, ctx.Err()
	}}
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return e, nil }
	if _, err := Run(ctx, in); !errors.Is(err, context.Canceled) || e.closes != 1 {
		t.Fatalf("cancel=%v e=%+v", err, e)
	}
	cp, _ := in.Store.LoadCheckpoint(t.Context())
	if cp.Phase != PhaseDispatch {
		t.Fatal(cp.Phase)
	}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || e.calls != 1 {
		t.Fatalf("resume=%+v %v e=%+v", result, err, e)
	}
}

func TestRunTerminalToolErrorsStopTheTail(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fatalError{errCrash}} {
		t.Run(err.Error(), func(t *testing.T) {
			in, _ := input(batch(complete("done"), call("never", "get_calls", `{"ids":["child"]}`)))
			c := in.Controller.(*controller)
			c.completeErr = err
			if _, got := Run(t.Context(), in); !errors.Is(got, err) {
				t.Fatalf("error=%v want=%v", got, err)
			}
			cp, _ := in.Store.LoadCheckpoint(t.Context())
			if cp.Phase != PhaseDispatch || cp.Next != 0 || strings.Join(c.log, ",") != "model,complete" {
				t.Fatalf("checkpoint=%+v log=%v", cp, c.log)
			}
		})
	}
}

func TestRunMissingDependenciesFailBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"model", func(in *Input) { in.Model = nil }}, {"store", func(in *Input) { in.Store = nil }},
		{"backend", func(in *Input) { in.Backend = nil }}, {"factory", func(in *Input) { in.ExecutorFactory = nil }},
		{"controller", func(in *Input) { in.Controller = nil }}, {"sink", func(in *Input) { in.Sink = nil }},
		{"redactor", func(in *Input) { in.Redactor = nil }}, {"limits", func(in *Input) { in.ModelLimits = session.ModelLimits{} }},
		{"budget", func(in *Input) { in.Definition.Budget = wire.AgentBudget{} }}, {"negative budget", func(in *Input) { in.Definition.Budget.Steps = -1 }},
		{"identity", func(in *Input) { in.Definition.ContractHash = "" }}, {"instructions", func(in *Input) { in.Definition.Instructions = "" }},
		{"output schema", func(in *Input) { in.Definition.OutputSchema = nil }}, {"message", func(in *Input) { in.Message = "" }},
		{"child contract", func(in *Input) { in.Definition.Subagents = []string{"missing"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(complete("done")))
			tc.mutate(&in)
			if _, err := Run(t.Context(), in); err == nil || len(model.DoStreamCalls) != 0 {
				t.Fatalf("error=%v calls=%d", err, len(model.DoStreamCalls))
			}
		})
	}
}

func TestCheckpointRejectsIncompatibleOrCorruptState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Checkpoint)
	}{
		{"version", func(cp *Checkpoint) { cp.Version = 2 }}, {"contract", func(cp *Checkpoint) { cp.ContractHash = "different" }},
		{"revision", func(cp *Checkpoint) { cp.Revision = 0 }}, {"phase", func(cp *Checkpoint) { cp.Phase = "yield" }},
		{"missing pending", func(cp *Checkpoint) { cp.Phase = PhaseDispatch }}, {"missing reply", func(cp *Checkpoint) { cp.Phase = PhaseCompleted }},
		{"cursor", func(cp *Checkpoint) { cp.Next = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(complete("done")))
			cp := &Checkpoint{Version: 1, Revision: 1, ContractHash: in.Definition.ContractHash, Phase: PhaseReady}
			tc.mutate(cp)
			in.Store.(*memoryStore).cp, _ = json.Marshal(cp)
			if _, err := Run(t.Context(), in); err == nil || len(model.DoStreamCalls) != 0 {
				t.Fatalf("error=%v calls=%d", err, len(model.DoStreamCalls))
			}
		})
	}
}

func TestMemoryStoreRevisionAndAtomicAppend(t *testing.T) {
	s := &memoryStore{}
	cp := &Checkpoint{Version: 1, Revision: 1, Phase: PhaseReady, Append: []session.Message{{ID: "user", Role: "user", Content: "one"}}}
	if err := s.SaveCheckpoint(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(t.Context(), cp); err != nil || len(s.messages) != 1 {
		t.Fatalf("retry=%v messages=%d", err, len(s.messages))
	}
	cp.Append[0].Content = "different"
	if err := s.SaveCheckpoint(t.Context(), cp); err == nil {
		t.Fatal("stale writer accepted")
	}
}
