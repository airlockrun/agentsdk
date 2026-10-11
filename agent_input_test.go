package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

func TestAgentInputAuthenticatedSnapshot(t *testing.T) {
	a, _ := testAgent(t)
	taskModel(a)
	var h *AgentHandle[taskInput, taskOutput]
	d := taskDefinition()
	d.Tools = []tool.Tool{tool.Typed[taskInput, taskOutput]("inspect").Description("Read admitted input").Execute(func(ctx context.Context, model taskInput) (taskOutput, error) {
		first, err := h.Input(ctx)
		if err != nil {
			return taskOutput{}, err
		}
		first.Task = "mutated"
		second, err := h.Input(ctx)
		if err != nil {
			return taskOutput{}, err
		}
		if second.Task == model.Task {
			t.Fatal("model arguments replaced admitted input")
		}
		return taskOutput{Answer: second.Task}, nil
	}).Build()}
	h = RegisterAgent(a, d)
	if _, err := h.Input(context.Background()); err == nil {
		t.Fatal("unrelated context gained input")
	}
	for _, original := range []string{"first", "second"} {
		r := runtimeRequest(a, "tool//inspect")
		r.Context.Caller = testWireCaller("application", wire.AccessAdmin)
		r.Context.Definition = &wire.RuntimeAgentDefinition{Slug: h.Slug(), ContractHash: h.ContractHash()}
		r.Context.TaskInput, _ = json.Marshal(taskInput{Task: original})
		r.Input = json.RawMessage(`{"task":"model spoof"}`)
		w := invokeRuntime(t, a.Handler(), r, a.token)
		var out wire.RuntimeInvokeResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Output != `{"answer":"`+original+`"}` {
			t.Fatalf("input scope lost: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestAgentInputRejectsForeignDefinitionAndRun(t *testing.T) {
	a, _ := testAgent(t)
	taskModel(a)
	h := RegisterAgent(a, taskDefinition())
	other := taskDefinition()
	other.Slug = "other"
	foreign := RegisterAgent(a, other)
	r := runtimeRequest(a, "tool//unused")
	r.Context.Caller = testWireCaller("application", wire.AccessAdmin)
	r.Context.Definition = &wire.RuntimeAgentDefinition{Slug: h.Slug(), ContractHash: h.ContractHash()}
	r.Context.TaskInput = json.RawMessage(`{"task":"original"}`)
	active := a.borrowRuntimeRun(t.Context(), r.Context)
	if _, err := foreign.Input(active.ctx); err == nil {
		t.Fatal("foreign definition gained input")
	}
	active.id = "another-run"
	if _, err := h.Input(active.ctx); err == nil {
		t.Fatal("run binding lost")
	}
	r.Context.Definition = nil
	if err := validateRuntimeContext(r.Context); err == nil {
		t.Fatal("unscoped original input accepted")
	}
}

func TestAgentInputFreshNestedValues(t *testing.T) {
	type input struct {
		Pages []int   `json:"pages"`
		Label *string `json:"label"`
	}
	a, _ := testAgent(t)
	taskModel(a)
	h := RegisterAgent(a, &AgentDefinition[input, taskOutput]{Slug: "nested", Description: "Nested input", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
	scope := runtimeRequest(a, "tool//unused").Context
	scope.Caller = testWireCaller("application", wire.AccessAdmin)
	scope.Definition = &wire.RuntimeAgentDefinition{Slug: h.Slug(), ContractHash: h.ContractHash()}
	scope.TaskInput = json.RawMessage(`{"pages":[9,10],"label":"original"}`)
	r := a.borrowRuntimeRun(t.Context(), scope)
	first, err := h.Input(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Pages[0] = 999
	*first.Label = "mutated"
	scope.TaskInput[0] = '!'
	second, err := h.Input(r.ctx)
	if err != nil || second.Pages[0] != 9 || *second.Label != "original" {
		t.Fatalf("mutable snapshot: %+v %v", second, err)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	if _, err := h.Input(ctx); err == nil {
		t.Fatal("cancelled callback read input")
	}
}
