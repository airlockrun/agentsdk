package localruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

func TestDeclaredChildWaitReleasesWorkerAndResumesJournal(t *testing.T) {
	childModel := completeModel(t)
	var h *Host
	var state *State
	turn := 0
	parentModel, err := testutil.NewMockModel(testutil.MockConfig{ID: "parent", Stream: func(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
		turn++
		call := stream.ToolCall{ID: "spawn", Name: "spawn_child", Input: json.RawMessage(`{}`)}
		if turn == 2 {
			var childID string
			if err := state.update(ctx, func(d *stateData) error {
				for id, task := range d.Tasks {
					if task.ParentID != "" {
						childID = id
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(wire.AgentWaitRequest{IDs: []string{childID}, Mode: "all"})
			call = stream.ToolCall{ID: "wait", Name: "wait_calls", Input: raw}
		}
		if turn == 3 {
			call = stream.ToolCall{ID: "done", Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"answer":"parent"}}`)}
		}
		model, err := testutil.NewMockModel(testutil.MockConfig{ID: "parent-response", Default: &testutil.MockResponse{ToolCalls: []stream.ToolCall{call}, Usage: stream.UsageFrom(1, 1)}})
		if err != nil {
			return nil, err
		}
		return model.Stream(ctx, opts)
	}})
	if err != nil {
		t.Fatal(err)
	}
	host, store, server, child := testHost(t, childModel)
	h = host
	state = store
	child.Slug = "child"
	child.ContractHash, err = wire.AgentDefinitionHash(child)
	if err != nil {
		t.Fatal(err)
	}
	parent := child
	parent.Slug = "parent"
	parent.ModelSlot = "parent_model"
	parent.Subagents = []string{"child"}
	parent.Budget.Steps = 6
	parent.MaxSubagentCalls = 2
	parent.MaxConcurrentSubagents = 1
	parent.ContractHash, err = wire.AgentDefinitionHash(parent)
	if err != nil {
		t.Fatal(err)
	}
	h.config.Models["parent_model"] = ModelBinding{Model: parentModel, Limits: session.ModelLimits{Context: 100000, Output: 4096}}
	if err := state.update(t.Context(), func(d *stateData) error {
		d.Manifest.ModelSlots = append(d.Manifest.ModelSlots, wire.ModelSlotDef{Slug: "parent_model", Capability: "text"})
		d.Manifest.AgentDefinitions = []wire.AgentDefinition{parent, child}
		return wire.ValidateAgentDefinitions(d.Manifest)
	}); err != nil {
		t.Fatal(err)
	}
	status, body := hostRequest(t, server, "POST", "/api/agent/agents/parent/runs", wire.StartAgentRequest{RequestID: uuid.NewString(), ContractHash: parent.ContractHash, Input: json.RawMessage(`{}`)}, nil)
	if status != 200 {
		t.Fatalf("start %d %s", status, body)
	}
	var started wire.AgentRunResponse
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		worked, err := h.WorkOnce(t.Context())
		if err != nil || !worked {
			t.Fatalf("worker %v %v", worked, err)
		}
	}
	if err := state.update(t.Context(), func(d *stateData) error {
		root := d.Tasks[started.Run.ID]
		if root.Info.Status != "completed" || root.Info.Steps != 4 || root.Info.Tokens != 8 || root.Attempts != 1 {
			t.Fatalf("root=%+v attempts=%d", root.Info, root.Attempts)
		}
		children := 0
		for _, task := range d.Tasks {
			if task.ParentID == root.Info.ID {
				children++
				if task.Info.Status != "completed" || task.Scope.Caller.User != nil {
					t.Fatalf("child=%+v", task)
				}
			}
		}
		if children != 1 {
			t.Fatalf("child count=%d", children)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if turn != 3 {
		t.Fatalf("parent model turns=%d", turn)
	}
}
