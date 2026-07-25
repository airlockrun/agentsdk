package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	sol "github.com/airlockrun/sol"
	solagent "github.com/airlockrun/sol/agent"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
)

type greetIn struct {
	Name string `json:"name"`
}

type greetOut struct {
	Greeting string `json:"greeting"`
}

// greetTool builds a greet-shaped tool.Tool for the test suite.
func greetTool(name, desc string, fn tool.TypedFunc[greetIn, greetOut]) tool.Tool {
	return tool.Typed[greetIn, greetOut](name).Description(desc).Execute(fn).Build()
}

func TestPromptHandler(t *testing.T) {
	a, mock := testAgent(t)
	_ = mock

	a.RegisterTool(greetTool("greet", "Returns a greeting.",
		func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "Hello, " + in.Name + "!"}, nil
		}), AccessUser)

	input := wire.PromptInput{
		Messages: []message.Message{
			message.NewUserMessage("say hello to World"),
		},
	}
	body, _ := json.Marshal(input)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/prompt", bytes.NewReader(body))
	r.Header.Set("X-Run-ID", "run-prompt-1")

	handlePrompt(a)(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Should have NDJSON response.
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"type"`) {
		t.Fatalf("expected NDJSON events, got: %s", respBody)
	}

	// Should have recorded run completion.
	completeReqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(completeReqs) != 1 {
		t.Fatalf("expected 1 complete request, got %d", len(completeReqs))
	}
}

// Failures return errors without re-suspending the parent.
func TestResumeInProcessChild_EarlyReturns(t *testing.T) {
	a, _ := testAgent(t)
	ew := newEventWriter(httptest.NewRecorder())

	t.Run("decode error", func(t *testing.T) {
		text, susp, err := resumeInProcessChild(context.Background(), a, "run-1",
			sol.RunnerOptions{}, json.RawMessage("not json"), true, "", ew)
		if susp != nil {
			t.Fatalf("decode error must not re-suspend, got %+v", susp)
		}
		if err == nil || !strings.Contains(err.Error(), "decode in-process child:") {
			t.Fatalf("error = %v, want decode error", err)
		}
		if text != "" {
			t.Fatalf("text = %q, want empty", text)
		}
	})

	t.Run("unknown subagent factory", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{
			"agentName":         "no-such-subagent-xyz",
			"messages":          []any{},
			"suspensionContext": permissionTestSuspension("call-1", stream.ToolCall{ID: "call-1", Name: "tool"}),
		})
		text, susp, err := resumeInProcessChild(context.Background(), a, "run-1",
			sol.RunnerOptions{}, raw, true, "", ew)
		if susp != nil {
			t.Fatalf("missing factory must not re-suspend, got %+v", susp)
		}
		if err == nil || !strings.Contains(err.Error(), "not found on resume") {
			t.Fatalf("error = %v, want missing factory error", err)
		}
		if text != "" {
			t.Fatalf("text = %q, want empty", text)
		}
	})
}

func TestPromptResumeCheckpointValidationPreventsWork(t *testing.T) {
	tests := []struct {
		name       string
		suspension *sol.SuspensionContext
		wantError  string
	}{
		{name: "nil", wantError: "suspension context is required"},
		{name: "question", suspension: &sol.SuspensionContext{Reason: "question"}, wantError: `reason \"question\" is not supported`},
		{name: "unknown", suspension: &sol.SuspensionContext{Reason: "other"}, wantError: `reason \"other\" is not supported`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newPermissionResumeServer(t, permissionRunCheckpoint{SuspensionContext: tt.suspension})
			defer fake.Close()
			agent := permissionResumeAgent(t, fake, []string{"must_not_run"})

			response := runPermissionResume(t, agent, true, "")
			if fake.ModelCalls() != 0 || len(fake.Executed()) != 0 {
				t.Fatalf("model calls = %d, executed = %v", fake.ModelCalls(), fake.Executed())
			}
			if got, want := fake.Order(), []string{"checkpoint", "complete"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("operation order = %v, want %v", got, want)
			}
			if !strings.Contains(response, tt.wantError) {
				t.Fatalf("validation error is missing: %s", response)
			}
		})
	}
}

func TestResumeInProcessChildCheckpointValidationPreventsWork(t *testing.T) {
	const agentName = "agentsdk-invalid-child-checkpoint-test"
	factoryCalls := 0
	if err := solagent.Register(agentName, func(string) *solagent.Agent {
		factoryCalls++
		return &solagent.Agent{Name: agentName, MaxSteps: 1}
	}); err != nil {
		t.Fatal(err)
	}
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{
		StreamResponse: testutil.MockTextResponse("must not run", testutil.MockUsage(1, 1)),
	})
	agent, _ := testAgent(t)

	for _, suspension := range []*sol.SuspensionContext{
		nil,
		{Reason: "question"},
		{Reason: "other"},
	} {
		factoryCalls = 0
		child, err := json.Marshal(sol.InProcessChild{AgentName: agentName, SuspensionContext: suspension})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = resumeInProcessChild(context.Background(), agent, "run-1", sol.RunnerOptions{
			Model: model,
			Bus:   bus.New(),
			Quiet: true,
		}, child, true, "", newEventWriter(httptest.NewRecorder()))
		if err == nil {
			t.Fatalf("suspension %#v: error = nil", suspension)
		}
		if factoryCalls != 0 || len(model.DoStreamCalls) != 0 {
			t.Fatalf("suspension %#v: factory calls = %d, model calls = %d", suspension, factoryCalls, len(model.DoStreamCalls))
		}
	}
}

func TestPromptPermissionResumeOrderingAndResuspension(t *testing.T) {
	calls := []stream.ToolCall{
		permissionRunJSCall("first", true),
		permissionRunJSCall("safe", false),
		permissionRunJSCall("second", true),
		permissionRunJSCall("tail", false),
	}
	history := permissionCheckpointMessages(calls)
	compaction := &sol.CompactionState{Messages: []goai.Message{goai.NewUserMessage("summary")}}
	fake := newPermissionResumeServer(t, permissionRunCheckpoint{
		Messages:          history,
		SuspensionContext: permissionTestSuspension("first", calls...),
		CompactionState:   compaction,
	})
	defer fake.Close()
	agent := permissionResumeAgent(t, fake, []string{"first", "safe", "second", "tail"})

	response := runPermissionResume(t, agent, true, "")
	if got, want := fake.Order(), []string{
		"checkpoint", "load", "execute:first", "append:first", "execute:safe", "append:safe", "complete",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
	if fake.ModelCalls() != 0 {
		t.Fatalf("model calls = %d, want 0", fake.ModelCalls())
	}
	if firstResult, nextGate := strings.Index(response, `"toolCallId":"first"`), strings.Index(response, `"toolCallId":"second"`); firstResult < 0 || nextGate < 0 || firstResult >= nextGate {
		t.Fatalf("result and next gate are missing or out of order: %s", response)
	}

	complete := fake.SingleCompletion(t)
	if complete.Status != "suspended" {
		t.Fatalf("completion status = %q, want suspended", complete.Status)
	}
	var checkpoint permissionRunCheckpoint
	if err := json.Unmarshal(complete.Checkpoint, &checkpoint); err != nil {
		t.Fatalf("decode completed checkpoint: %v", err)
	}
	if checkpoint.SuspensionContext == nil || checkpoint.SuspensionContext.ToolCallID != "second" {
		t.Fatalf("next suspension = %#v, want second", checkpoint.SuspensionContext)
	}
	if len(checkpoint.Messages) != len(history)+2 {
		t.Fatalf("checkpoint messages = %d, want %d", len(checkpoint.Messages), len(history)+2)
	}
	if !reflect.DeepEqual(checkpoint.CompactionState, compaction) {
		t.Fatalf("compaction state = %#v, want %#v", checkpoint.CompactionState, compaction)
	}
}

func TestPromptPermissionDenialSkipsTail(t *testing.T) {
	calls := []stream.ToolCall{
		permissionRunJSCall("first", true),
		permissionRunJSCall("tail", false),
	}
	fake := newPermissionResumeServer(t, permissionRunCheckpoint{
		Messages:          permissionCheckpointMessages(calls),
		SuspensionContext: permissionTestSuspension("first", calls...),
	})
	defer fake.Close()
	agent := permissionResumeAgent(t, fake, []string{"first", "tail"})

	response := runPermissionResume(t, agent, false, "")
	for _, operation := range fake.Order() {
		if strings.HasPrefix(operation, "execute:") {
			t.Fatalf("denial executed a tool: %v", fake.Order())
		}
	}
	if fake.ModelCalls() != 1 {
		t.Fatalf("model calls = %d, want 1", fake.ModelCalls())
	}
	if strings.Count(response, `"type":"tool-output-denied"`) != 2 {
		t.Fatalf("denied result count is not 2: %s", response)
	}
	if !strings.Contains(response, "earlier tool call in the same ordered batch was denied") {
		t.Fatalf("tail skip reason is missing: %s", response)
	}
}

func TestPromptPermissionResumeFailuresDoNotInvokeModel(t *testing.T) {
	calls := []stream.ToolCall{permissionRunJSCall("first", true)}
	checkpoint := permissionRunCheckpoint{
		Messages:          permissionCheckpointMessages(calls),
		SuspensionContext: permissionTestSuspension("first", calls...),
	}

	t.Run("checkpoint fetch", func(t *testing.T) {
		fake := newPermissionResumeServer(t, checkpoint)
		defer fake.Close()
		fake.checkpointStatus = http.StatusInternalServerError
		agent := permissionResumeAgent(t, fake, []string{"first"})
		response := runPermissionResume(t, agent, true, "")
		if fake.ModelCalls() != 0 || len(fake.Executed()) != 0 {
			t.Fatalf("model calls = %d, executed = %v", fake.ModelCalls(), fake.Executed())
		}
		if !strings.Contains(response, "load resume checkpoint") {
			t.Fatalf("checkpoint error is missing: %s", response)
		}
	})

	t.Run("checkpoint decode", func(t *testing.T) {
		fake := newPermissionResumeServer(t, checkpoint)
		defer fake.Close()
		fake.checkpointRaw = []byte(`{"messages":`)
		agent := permissionResumeAgent(t, fake, []string{"first"})
		runPermissionResume(t, agent, true, "")
		if fake.ModelCalls() != 0 || len(fake.Executed()) != 0 {
			t.Fatalf("model calls = %d, executed = %v", fake.ModelCalls(), fake.Executed())
		}
	})

	t.Run("result append", func(t *testing.T) {
		fake := newPermissionResumeServer(t, checkpoint)
		defer fake.Close()
		fake.appendFailAt = 1
		agent := permissionResumeAgent(t, fake, []string{"first"})
		response := runPermissionResume(t, agent, true, "")
		if fake.ModelCalls() != 0 {
			t.Fatalf("model calls = %d, want 0", fake.ModelCalls())
		}
		if got, want := fake.Order(), []string{"checkpoint", "load", "execute:first", "append:first", "complete"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("operation order = %v, want %v", got, want)
		}
		if !strings.Contains(response, "store append tool result") {
			t.Fatalf("append error is missing: %s", response)
		}
	})
}

func TestPromptSuspendedCompletionFailureIsEmitted(t *testing.T) {
	calls := []stream.ToolCall{
		permissionRunJSCall("first", true),
		permissionRunJSCall("second", true),
	}
	fake := newPermissionResumeServer(t, permissionRunCheckpoint{
		Messages:          permissionCheckpointMessages(calls),
		SuspensionContext: permissionTestSuspension("first", calls...),
	})
	defer fake.Close()
	fake.completeStatus = http.StatusInternalServerError
	agent := permissionResumeAgent(t, fake, []string{"first", "second"})

	response := runPermissionResume(t, agent, true, "")
	if fake.ModelCalls() != 0 {
		t.Fatalf("model calls = %d, want 0", fake.ModelCalls())
	}
	if !strings.Contains(response, "complete suspended run") {
		t.Fatalf("completion error is missing: %s", response)
	}
}

func TestResumeInProcessChildPermissionCarriesNoStoreResults(t *testing.T) {
	const agentName = "agentsdk-resume-permission-history-test"
	call := stream.ToolCall{ID: "write-1", Name: "write", Input: json.RawMessage(`{}`)}
	writeTool := tool.New("write").Description("write").Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
		return tool.Result{Output: "written"}, nil
	}).Build()
	if err := solagent.Register(agentName, func(string) *solagent.Agent {
		return &solagent.Agent{Name: agentName, Tools: tool.Set{"write": writeTool}, MaxSteps: 1}
	}); err != nil {
		t.Fatal(err)
	}
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{
		StreamResponse: testutil.MockTextResponse("continued", testutil.MockUsage(1, 1)),
	})
	child, err := json.Marshal(sol.InProcessChild{
		AgentName: agentName,
		Messages: []goai.Message{
			goai.NewUserMessage("write it"),
			goai.NewAssistantMessageWithParts(goai.ToolCallPart{ID: call.ID, Name: call.Name, Input: call.Input}),
		},
		SuspensionContext: permissionTestSuspension(call.ID, call),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := testAgent(t)
	text, suspension, err := resumeInProcessChild(context.Background(), agent, "run-1", sol.RunnerOptions{
		Model: model,
		Bus:   bus.New(),
		Quiet: true,
	}, child, true, "", newEventWriter(httptest.NewRecorder()))
	if err != nil {
		t.Fatalf("resumeInProcessChild() error = %v", err)
	}
	if suspension != nil {
		t.Fatalf("suspension = %#v, want nil", suspension)
	}
	if text != "continued" {
		t.Fatalf("text = %q, want continued", text)
	}
	if len(model.DoStreamCalls) != 1 || !modelCallHasToolResult(model.DoStreamCalls[0].Messages, call.ID) {
		t.Fatalf("model history does not contain tool result: %#v", model.DoStreamCalls)
	}
}

func TestResolveDelegatedSuspensionPersistenceOrdering(t *testing.T) {
	const agentName = "agentsdk-delegated-persistence-order-test"
	var order []string
	childTool := tool.New("child_write").Description("write").Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
		order = append(order, "execute")
		return tool.Result{Output: "written"}, nil
	}).Build()
	if err := solagent.Register(agentName, func(string) *solagent.Agent {
		return &solagent.Agent{Name: agentName, Tools: tool.Set{"child_write": childTool}, MaxSteps: 1}
	}); err != nil {
		t.Fatal(err)
	}
	call := stream.ToolCall{ID: "child-call", Name: "child_write", Input: json.RawMessage(`{}`)}
	child := sol.InProcessChild{
		AgentName: agentName,
		Messages: []goai.Message{
			goai.NewUserMessage("write"),
			goai.NewAssistantMessageWithParts(goai.ToolCallPart{ID: call.ID, Name: call.Name, Input: call.Input}),
		},
		SuspensionContext: permissionTestSuspension(call.ID, call),
	}
	parent := &sol.SuspensionContext{
		Reason:     "delegated",
		ToolCallID: "parent-call",
		Data: &bus.ErrDelegatedSuspend{
			ToolCallID: "parent-call",
			Transport:  "inprocess",
			Child:      child,
		},
		PendingToolCalls: []stream.ToolCall{
			{ID: "parent-call", Name: "promptAgent"},
			{ID: "other-call", Name: "other"},
		},
	}
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{
		StreamResponse: testutil.MockTextResponse("child complete", testutil.MockUsage(1, 1)),
	})
	agent, _ := testAgent(t)

	t.Run("load execute append emit", func(t *testing.T) {
		order = nil
		store := &delegatedOrderingStore{order: &order}
		recorder := httptest.NewRecorder()
		resolution, err := resolveDelegatedSuspension(context.Background(), agent, "run-1", sol.RunnerOptions{
			Model: model,
			Bus:   bus.New(),
			Quiet: true,
		}, parent, true, "", store, newEventWriter(recorder))
		if err != nil {
			t.Fatalf("resolveDelegatedSuspension() error = %v", err)
		}
		if resolution.SuspensionContext != nil || len(resolution.Messages) != 1 {
			t.Fatalf("resolution = %#v", resolution)
		}
		if want := []string{"load", "execute", "append"}; !reflect.DeepEqual(order, want) {
			t.Fatalf("operation order = %v, want %v", order, want)
		}
		if !strings.Contains(recorder.Body.String(), `"toolCallId":"parent-call"`) {
			t.Fatalf("delegated result was not emitted: %s", recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"toolCallId":"other-call"`) || len(store.appended) != 1 ||
			len(store.appended[0].Parts) != 1 || store.appended[0].Parts[0].Tool == nil ||
			store.appended[0].Parts[0].Tool.CallID != "parent-call" {
			t.Fatalf("unexpected delegated results: appended=%#v events=%s", store.appended, recorder.Body.String())
		}
	})

	t.Run("append failure suppresses emit", func(t *testing.T) {
		order = nil
		store := &delegatedOrderingStore{order: &order, appendErr: errors.New("revision conflict")}
		recorder := httptest.NewRecorder()
		_, err := resolveDelegatedSuspension(context.Background(), agent, "run-1", sol.RunnerOptions{
			Model: model,
			Bus:   bus.New(),
			Quiet: true,
		}, parent, true, "", store, newEventWriter(recorder))
		if err == nil || !strings.Contains(err.Error(), "revision conflict") {
			t.Fatalf("error = %v, want revision conflict", err)
		}
		if want := []string{"load", "execute", "append"}; !reflect.DeepEqual(order, want) {
			t.Fatalf("operation order = %v, want %v", order, want)
		}
		if recorder.Body.Len() != 0 {
			t.Fatalf("result emitted before failed append: %s", recorder.Body.String())
		}
	})
}

func TestResolveDelegatedSuspensionValidatesCurrentCallBeforeWork(t *testing.T) {
	agent, _ := testAgent(t)
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{
		StreamResponse: testutil.MockTextResponse("must not run", testutil.MockUsage(1, 1)),
	})
	tests := []struct {
		name      string
		outerID   string
		dataID    string
		pending   []stream.ToolCall
		wantError string
	}{
		{
			name:      "context and data mismatch",
			outerID:   "outer-call",
			dataID:    "data-call",
			pending:   []stream.ToolCall{{ID: "data-call", Name: "promptAgent"}},
			wantError: "does not match data tool call ID",
		},
		{
			name:      "current call is not first",
			outerID:   "current-call",
			dataID:    "current-call",
			pending:   []stream.ToolCall{{ID: "other-call", Name: "other"}, {ID: "current-call", Name: "promptAgent"}},
			wantError: "is not first pending call",
		},
		{
			name:      "data ID missing",
			outerID:   "current-call",
			pending:   []stream.ToolCall{{ID: "current-call", Name: "promptAgent"}},
			wantError: "has no tool call ID",
		},
		{
			name:      "pending calls missing",
			outerID:   "current-call",
			dataID:    "current-call",
			wantError: "has no pending tool calls",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var order []string
			store := &delegatedOrderingStore{order: &order}
			recorder := httptest.NewRecorder()
			suspension := &sol.SuspensionContext{
				Reason:           "delegated",
				ToolCallID:       tt.outerID,
				Data:             &bus.ErrDelegatedSuspend{ToolCallID: tt.dataID, Transport: "inprocess", Child: json.RawMessage(`{}`)},
				PendingToolCalls: tt.pending,
			}
			_, err := resolveDelegatedSuspension(context.Background(), agent, "run-1", sol.RunnerOptions{
				Model: model,
				Bus:   bus.New(),
				Quiet: true,
			}, suspension, true, "", store, newEventWriter(recorder))
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
			if len(order) != 0 || recorder.Body.Len() != 0 || len(model.DoStreamCalls) != 0 {
				t.Fatalf("validation caused work: order=%v output=%q model calls=%d", order, recorder.Body.String(), len(model.DoStreamCalls))
			}
		})
	}
}

func TestDelegatedInProcessResuspensionHasOneActionableConfirmation(t *testing.T) {
	const agentName = "agentsdk-scoped-child-bus-test"
	calls := registerResuspendingChild(t, agentName)
	parent := delegatedInProcessSuspension(t, agentName, calls)
	parentBus := bus.New()
	recorder := httptest.NewRecorder()
	ew := newEventWriter(recorder)
	unsub := streamBusToNDJSON(parentBus, ew)
	defer unsub()
	model := testutil.NewMockLanguageModel(testutil.MockLanguageModelOptions{
		StreamResponse: testutil.MockTextResponse("must not run", testutil.MockUsage(1, 1)),
	})
	agent, _ := testAgent(t)

	resolution, err := resolveDelegatedSuspension(context.Background(), agent, "run-1", sol.RunnerOptions{
		Model: model,
		Bus:   parentBus,
		Quiet: true,
	}, parent, true, "", nil, ew)
	if err != nil {
		t.Fatalf("resolveDelegatedSuspension() error = %v", err)
	}
	if resolution.SuspensionContext == nil {
		t.Fatal("resolution suspension = nil")
	}
	emitSuspensionEvent(ew, resolution.SuspensionContext)

	type confirmationEvent struct {
		Type string `json:"type"`
		Data struct {
			Permission string   `json:"permission"`
			Patterns   []string `json:"patterns"`
			Code       string   `json:"code"`
			ToolCallID string   `json:"toolCallId"`
		} `json:"data"`
	}
	var confirmations []confirmationEvent
	for _, line := range strings.Split(strings.TrimSpace(recorder.Body.String()), "\n") {
		var event confirmationEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		if event.Type == "confirmation_required" {
			confirmations = append(confirmations, event)
		}
	}
	if len(confirmations) != 1 {
		t.Fatalf("confirmation count = %d, want 1: %s", len(confirmations), recorder.Body.String())
	}
	confirmation := confirmations[0].Data
	if confirmation.Permission != agentName+": second-permission" ||
		!reflect.DeepEqual(confirmation.Patterns, []string{"/next"}) ||
		confirmation.Code != "next code" || confirmation.ToolCallID != "parent-call" {
		t.Fatalf("confirmation = %#v", confirmation)
	}
	if len(model.DoStreamCalls) != 0 {
		t.Fatalf("model calls = %d, want 0", len(model.DoStreamCalls))
	}
}

func TestPromptDelegatedResuspensionPersistsHumanMessage(t *testing.T) {
	const agentName = "agentsdk-durable-resume-message-test"
	calls := registerResuspendingChild(t, agentName)
	parent := delegatedInProcessSuspension(t, agentName, calls)
	history := permissionCheckpointMessages(parent.PendingToolCalls)

	t.Run("store", func(t *testing.T) {
		fake := newPermissionResumeServer(t, permissionRunCheckpoint{Messages: history, SuspensionContext: parent})
		defer fake.Close()
		agent := permissionResumeAgent(t, fake, nil)

		response := runPermissionResume(t, agent, true, "Please continue with that")
		if got, want := fake.Order(), []string{"checkpoint", "load", "append:user", "complete"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("operation order = %v, want %v", got, want)
		}
		assertDelegatedResumeMessage(t, fake.SingleCompletion(t), history, "Please continue with that")
		stored := fake.History()
		if len(stored) != len(history)+1 || stored[len(stored)-1].Role != "user" || stored[len(stored)-1].Content != "Please continue with that" {
			t.Fatalf("stored history = %#v", stored)
		}
		if !strings.Contains(response, `"type":"messages"`) {
			t.Fatalf("message event is missing: %s", response)
		}
	})

	t.Run("no store", func(t *testing.T) {
		fake := newPermissionResumeServer(t, permissionRunCheckpoint{Messages: history, SuspensionContext: parent})
		defer fake.Close()
		agent := permissionResumeAgent(t, fake, nil)

		runResume(t, agent, wire.PromptInput{
			ResumeRunID: "suspended-run",
			Approved:    boolPointer(true),
			Message:     "Keep going",
		})
		if got, want := fake.Order(), []string{"checkpoint", "complete"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("operation order = %v, want %v", got, want)
		}
		assertDelegatedResumeMessage(t, fake.SingleCompletion(t), history, "Keep going")
	})

	t.Run("append failure", func(t *testing.T) {
		fake := newPermissionResumeServer(t, permissionRunCheckpoint{Messages: history, SuspensionContext: parent})
		defer fake.Close()
		fake.appendFailAt = 1
		agent := permissionResumeAgent(t, fake, nil)

		response := runPermissionResume(t, agent, true, "Persist me")
		if !strings.Contains(response, "store append delegated resume message") {
			t.Fatalf("append error is missing: %s", response)
		}
		completion := fake.SingleCompletion(t)
		if completion.Status != "error" {
			t.Fatalf("completion status = %q, want error", completion.Status)
		}
		if strings.Contains(response, `"type":"confirmation_required"`) {
			t.Fatalf("confirmation emitted after failed append: %s", response)
		}
	})
}

type permissionRunCheckpoint struct {
	Messages          []goai.Message         `json:"messages"`
	SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
	CompactionState   *sol.CompactionState   `json:"compactionState"`
}

type delegatedOrderingStore struct {
	order     *[]string
	loaded    bool
	appendErr error
	appended  []session.Message
}

func (s *delegatedOrderingStore) Load(context.Context) ([]session.Message, error) {
	*s.order = append(*s.order, "load")
	s.loaded = true
	return nil, nil
}

func (s *delegatedOrderingStore) Append(_ context.Context, messages []session.Message) error {
	if !s.loaded {
		return errors.New("append without load")
	}
	*s.order = append(*s.order, "append")
	s.appended = append(s.appended, messages...)
	return s.appendErr
}

func (*delegatedOrderingStore) Compact(context.Context, []session.Message, int) error { return nil }

type permissionResumeServer struct {
	t                *testing.T
	server           *httptest.Server
	checkpoint       permissionRunCheckpoint
	checkpointRaw    []byte
	checkpointStatus int
	appendFailAt     int
	completeStatus   int
	mu               sync.Mutex
	order            []string
	executed         []string
	modelCalls       int
	appendCalls      int
	revision         int
	history          []session.Message
	completions      []wire.RunCompleteRequest
}

func newPermissionResumeServer(t *testing.T, checkpoint permissionRunCheckpoint) *permissionResumeServer {
	t.Helper()
	f := &permissionResumeServer{t: t, checkpoint: checkpoint, revision: 1}
	for _, msg := range checkpoint.Messages {
		f.history = append(f.history, session.FromGoAIMessage(msg))
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	return f
}

func (f *permissionResumeServer) Close() { f.server.Close() }

func (f *permissionResumeServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/agent/run/suspended-run/checkpoint":
		f.record("checkpoint")
		if f.checkpointStatus != 0 {
			http.Error(w, "checkpoint unavailable", f.checkpointStatus)
			return
		}
		if f.checkpointRaw != nil {
			_, _ = w.Write(f.checkpointRaw)
			return
		}
		_ = json.NewEncoder(w).Encode(f.checkpoint)
	case r.Method == http.MethodGet && r.URL.Path == "/api/agent/session/conversation/messages":
		f.mu.Lock()
		f.order = append(f.order, "load")
		response := wire.SessionLoadResponse{Messages: append([]session.Message(nil), f.history...), Revision: fmt.Sprintf("rev-%d", f.revision)}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(response)
	case r.Method == http.MethodPost && r.URL.Path == "/api/agent/session/conversation/messages":
		var request wire.SessionAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode append request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		appendName := request.Messages[0].Role
		if len(request.Messages[0].Parts) > 0 && request.Messages[0].Parts[0].Tool != nil {
			appendName = request.Messages[0].Parts[0].Tool.CallID
		}
		f.mu.Lock()
		f.order = append(f.order, "append:"+appendName)
		f.appendCalls++
		appendCall := f.appendCalls
		wantRevision := fmt.Sprintf("rev-%d", f.revision)
		if request.Revision != wantRevision {
			f.t.Errorf("append revision = %q, want %q", request.Revision, wantRevision)
		}
		if appendCall != f.appendFailAt {
			f.history = append(f.history, request.Messages...)
			f.revision++
		}
		revision := fmt.Sprintf("rev-%d", f.revision)
		f.mu.Unlock()
		if appendCall == f.appendFailAt {
			http.Error(w, "append failed", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(wire.SessionAppendResponse{Revision: revision})
	case r.Method == http.MethodPost && r.URL.Path == "/api/agent/llm/stream":
		f.mu.Lock()
		f.order = append(f.order, "model")
		f.modelCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"type\":\"start\",\"data\":{}}\n{\"type\":\"text-delta\",\"data\":{\"text\":\"continued\"}}\n{\"type\":\"finish\",\"data\":{\"finishReason\":\"stop\",\"usage\":{\"inputTokens\":{\"total\":1},\"outputTokens\":{\"total\":1}}}}\n"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/agent/run/complete":
		var request wire.RunCompleteRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode completion: %v", err)
		}
		f.mu.Lock()
		f.order = append(f.order, "complete")
		f.completions = append(f.completions, request)
		f.mu.Unlock()
		if f.completeStatus != 0 {
			http.Error(w, "completion failed", f.completeStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *permissionResumeServer) record(operation string) {
	f.mu.Lock()
	f.order = append(f.order, operation)
	if strings.HasPrefix(operation, "execute:") {
		f.executed = append(f.executed, strings.TrimPrefix(operation, "execute:"))
	}
	f.mu.Unlock()
}

func (f *permissionResumeServer) Order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *permissionResumeServer) Executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.executed...)
}

func (f *permissionResumeServer) ModelCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelCalls
}

func (f *permissionResumeServer) History() []session.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]session.Message(nil), f.history...)
}

func (f *permissionResumeServer) SingleCompletion(t *testing.T) wire.RunCompleteRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.completions) != 1 {
		t.Fatalf("completions = %d, want 1", len(f.completions))
	}
	return f.completions[0]
}

func permissionResumeAgent(t *testing.T, fake *permissionResumeServer, registeredNames []string) *Agent {
	t.Helper()
	agent, _ := testAgent(t)
	agent.httpClient = fake.server.Client()
	agent.client = newAirlockClient(fake.server.URL, "test-token", agent.httpClient)
	for _, name := range registeredNames {
		name := name
		registered := tool.New(name).Description(name).Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
			fake.record("execute:" + name)
			return tool.Result{Output: name + " complete"}, nil
		}).Build()
		agent.RegisterTool(registered, AccessUser)
	}
	return agent
}

func runPermissionResume(t *testing.T, agent *Agent, approved bool, messageText string) string {
	t.Helper()
	return runResume(t, agent, wire.PromptInput{
		ConversationID: "conversation",
		ResumeRunID:    "suspended-run",
		Approved:       &approved,
		Message:        messageText,
	})
}

func runResume(t *testing.T, agent *Agent, input wire.PromptInput) string {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/prompt", bytes.NewReader(body))
	r.Header.Set("X-Run-ID", "resume-run")
	handlePrompt(agent)(w, r)
	return w.Body.String()
}

func boolPointer(value bool) *bool {
	return &value
}

func registerResuspendingChild(t *testing.T, agentName string) []stream.ToolCall {
	t.Helper()
	first := tool.New("first").Description("first").Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
		return tool.Result{Output: "first complete"}, nil
	}).Build()
	second := tool.New("second").Description("second").Execute(func(ctx context.Context, _ json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		err := bus.PermissionManagerFromContext(ctx).Ask(ctx, bus.PermissionRequest{
			Permission: "second-permission",
			Patterns:   []string{"/next"},
			Metadata:   map[string]any{"code": "next code"},
			ToolCallID: opts.ToolCallID,
		})
		return tool.Result{}, err
	}).Build()
	if err := solagent.Register(agentName, func(string) *solagent.Agent {
		return &solagent.Agent{
			Name:     agentName,
			Tools:    tool.Set{"first": first, "second": second},
			MaxSteps: 1,
		}
	}); err != nil {
		t.Fatal(err)
	}
	return []stream.ToolCall{
		{ID: "child-first", Name: "first", Input: json.RawMessage(`{}`)},
		{ID: "child-second", Name: "second", Input: json.RawMessage(`{}`)},
	}
}

func delegatedInProcessSuspension(t *testing.T, agentName string, calls []stream.ToolCall) *sol.SuspensionContext {
	t.Helper()
	child := sol.InProcessChild{
		AgentName: agentName,
		Messages: []goai.Message{
			goai.NewUserMessage("run both"),
			goai.NewAssistantMessageWithParts(
				goai.ToolCallPart{ID: calls[0].ID, Name: calls[0].Name, Input: calls[0].Input},
				goai.ToolCallPart{ID: calls[1].ID, Name: calls[1].Name, Input: calls[1].Input},
			),
		},
		SuspensionContext: permissionTestSuspension(calls[0].ID, calls...),
	}
	return &sol.SuspensionContext{
		Reason:     "delegated",
		ToolCallID: "parent-call",
		Data: &bus.ErrDelegatedSuspend{
			ToolCallID: "parent-call",
			Transport:  "inprocess",
			Child:      child,
		},
		PendingToolCalls: []stream.ToolCall{{ID: "parent-call", Name: "promptAgent"}},
	}
}

func assertDelegatedResumeMessage(t *testing.T, completion wire.RunCompleteRequest, history []goai.Message, text string) {
	t.Helper()
	if completion.Status != "suspended" {
		t.Fatalf("completion status = %q, want suspended", completion.Status)
	}
	var checkpoint permissionRunCheckpoint
	if err := json.Unmarshal(completion.Checkpoint, &checkpoint); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if len(checkpoint.Messages) != len(history)+1 {
		t.Fatalf("checkpoint messages = %d, want %d", len(checkpoint.Messages), len(history)+1)
	}
	last := checkpoint.Messages[len(checkpoint.Messages)-1]
	if last.Role != "user" || last.Content.Text != text {
		t.Fatalf("last checkpoint message = %#v, want user %q", last, text)
	}
}

func permissionCheckpointMessages(calls []stream.ToolCall) []goai.Message {
	parts := make([]message.Part, len(calls))
	for i, call := range calls {
		parts[i] = goai.ToolCallPart{ID: call.ID, Name: call.Name, Input: call.Input}
	}
	return []goai.Message{
		goai.NewUserMessage("run the tools"),
		goai.NewAssistantMessageWithParts(parts...),
	}
}

func permissionRunJSCall(id string, confirmation bool) stream.ToolCall {
	input, err := json.Marshal(runJSInput{
		Code:                "tools." + id + "({})",
		Description:         "Run " + id,
		RequestConfirmation: confirmation,
	})
	if err != nil {
		panic(err)
	}
	return stream.ToolCall{ID: id, Name: "run_js", Input: input}
}

func permissionTestSuspension(currentID string, calls ...stream.ToolCall) *sol.SuspensionContext {
	return &sol.SuspensionContext{
		Reason:           "permission",
		ToolCallID:       currentID,
		Data:             &bus.ErrPermissionNeeded{Permission: currentID, ToolCallID: currentID},
		PendingToolCalls: calls,
	}
}

func modelCallHasToolResult(messages []goai.Message, callID string) bool {
	for _, msg := range messages {
		if msg.Role != "tool" || !msg.Content.IsMultiPart() {
			continue
		}
		for _, part := range msg.Content.Parts {
			if result, ok := part.(message.ToolResultPart); ok && result.ToolCallID == callID {
				return true
			}
		}
	}
	return false
}
