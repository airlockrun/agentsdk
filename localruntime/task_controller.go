package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/google/uuid"
)

type taskController struct {
	host      *Host
	id, owner string
	def       wire.AgentDefinition
}

func (c *taskController) BeforeModel(ctx context.Context) error {
	return c.update(ctx, func(d *stateData, t *taskRecord) error {
		records := []*taskRecord{t}
		if t.ParentID != "" {
			records = append(records, d.Tasks[t.ParentID])
		}
		for _, record := range records {
			def, err := definition(d, record.Info.Definition, record.Info.ContractHash)
			if err != nil {
				return err
			}
			if def.Budget.Steps > 0 && record.Info.Steps >= def.Budget.Steps || def.Budget.Tokens > 0 && record.Info.Tokens >= def.Budget.Tokens {
				return errors.New("localruntime: reasoning budget exceeded")
			}
		}
		for _, record := range records {
			record.Info.Steps++
		}
		return nil
	})
}

func (c *taskController) charge(ctx context.Context, usage stream.Usage) error {
	return c.update(ctx, func(d *stateData, t *taskRecord) error {
		records := []*taskRecord{t}
		if t.ParentID != "" {
			records = append(records, d.Tasks[t.ParentID])
		}
		exceeded := false
		for _, record := range records {
			def, err := definition(d, record.Info.Definition, record.Info.ContractHash)
			if err != nil {
				return err
			}
			tokens := int64(usage.GrandTotal())
			if tokens > math.MaxInt64-record.Info.Tokens {
				record.Info.Tokens = math.MaxInt64
			} else {
				record.Info.Tokens += tokens
			}
			if def.Budget.Tokens > 0 && (usage.InputTokens.Total == nil || usage.OutputTokens.Total == nil || record.Info.Tokens >= def.Budget.Tokens) {
				exceeded = true
			}
		}
		// Persist charged usage even when the limit is crossed; dispatch is gated by
		// the subsequent BeforeModel/Complete check and model stream error.
		if exceeded {
			t.Info.Error = "localruntime: token budget exceeded"
		}
		return nil
	})
}

func (c *taskController) Spawn(ctx context.Context, callID, slug string, input json.RawMessage) (out wire.AgentRunInfo, err error) {
	err = c.update(ctx, func(d *stateData, t *taskRecord) error {
		declared := false
		for _, child := range c.def.Subagents {
			if child == slug {
				declared = true
			}
		}
		if !declared || callID == "" {
			return errors.New("localruntime: child definition is not declared")
		}
		calls, active := int32(0), int32(0)
		reqID := uuid.NewSHA1(uuid.MustParse(c.id), []byte(callID)).String()
		for _, child := range d.Tasks {
			if child.ParentID == c.id {
				calls++
				if child.Info.CompletedAt == nil {
					active++
				}
				if child.RequestID == reqID {
					if !equalJSON([]byte(child.Input), input) || child.Info.Definition != slug {
						return errors.New("localruntime: child call payload conflict")
					}
					out = clone(child.Info)
					return nil
				}
			}
		}
		if calls >= c.def.MaxSubagentCalls || active >= c.def.MaxConcurrentSubagents {
			return errors.New("localruntime: child call budget exceeded")
		}
		for _, def := range d.Manifest.AgentDefinitions {
			if def.Slug == slug {
				resp, e := c.host.enqueue(d, slug, wire.StartAgentRequest{RequestID: reqID, ContractHash: def.ContractHash, Input: input}, "", c.id)
				out = resp.Run
				return e
			}
		}
		return errors.New("localruntime: child contract unavailable")
	})
	return
}

func (c *taskController) Continue(ctx context.Context, callID, sessionID, prompt string) (out wire.AgentRunInfo, err error) {
	err = c.update(ctx, func(d *stateData, t *taskRecord) error {
		for _, child := range d.Tasks {
			if child.ParentID == c.id && child.Info.SessionID == sessionID {
				raw, _ := json.Marshal(prompt)
				resp, e := c.host.enqueue(d, child.Info.Definition, wire.StartAgentRequest{RequestID: uuid.NewSHA1(uuid.MustParse(c.id), []byte(callID)).String(), ContractHash: child.Info.ContractHash, Input: raw}, sessionID, c.id)
				out = resp.Run
				return e
			}
		}
		return errors.New("localruntime: child session is not owned by this task")
	})
	return
}

func ownedCalls(d *stateData, parent string, ids []string) ([]wire.AgentRunInfo, error) {
	out := make([]wire.AgentRunInfo, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		t := d.Tasks[id]
		if t == nil || t.ParentID != parent || seen[id] {
			return nil, errors.New("localruntime: invalid child task selection")
		}
		seen[id] = true
		out = append(out, clone(t.Info))
	}
	return out, nil
}
func (c *taskController) Get(ctx context.Context, ids []string) (out []wire.AgentRunInfo, err error) {
	err = c.update(ctx, func(d *stateData, _ *taskRecord) error { var e error; out, e = ownedCalls(d, c.id, ids); return e })
	return
}
func (c *taskController) Cancel(ctx context.Context, ids []string) (out wire.AgentCancelResult, err error) {
	err = c.update(ctx, func(d *stateData, _ *taskRecord) error {
		calls, e := ownedCalls(d, c.id, ids)
		if e != nil {
			return e
		}
		for _, call := range calls {
			cancelRecord(d, d.Tasks[call.ID])
			out.Calls = append(out.Calls, clone(d.Tasks[call.ID].Info))
		}
		return nil
	})
	return
}

func (c *taskController) Wait(ctx context.Context, callID string, req wire.AgentWaitRequest) (out wire.AgentWaitResult, err error) {
	waiting := false
	err = c.update(ctx, func(d *stateData, t *taskRecord) error {
		if len(req.IDs) == 0 || (req.Mode != "all" && req.Mode != "any") {
			return errors.New("localruntime: wait requires IDs and all/any mode")
		}
		calls, e := ownedCalls(d, c.id, req.IDs)
		if e != nil {
			return e
		}
		now := time.Now()
		if t.WaitID != callID {
			t.WaitID = callID
			t.WaitRequest = clone(req)
			t.WaitDeadline = nil
			if req.TimeoutMS != nil {
				deadline := now.Add(time.Duration(*req.TimeoutMS) * time.Millisecond)
				t.WaitDeadline = &deadline
			}
		}
		for _, call := range calls {
			if call.CompletedAt != nil {
				out.Completed = append(out.Completed, call)
			} else {
				out.Pending = append(out.Pending, call.ID)
			}
		}
		if len(out.Pending) == 0 || req.Mode == "any" && len(out.Completed) > 0 {
			out.Reason = "completed"
			return nil
		}
		if t.WaitDeadline != nil && !now.Before(*t.WaitDeadline) {
			out.Reason = "timeout"
			return nil
		}
		waiting = true
		return nil
	})
	if err == nil && waiting {
		err = agentruntime.ErrWaiting
	}
	return
}
func (c *taskController) Complete(ctx context.Context, _ wire.AgentReply) error {
	return c.update(ctx, func(d *stateData, t *taskRecord) error {
		if t.Info.Error != "" {
			return errors.New(t.Info.Error)
		}
		if c.def.Budget.Tokens > 0 && t.Info.Tokens >= c.def.Budget.Tokens {
			return errors.New("localruntime: token budget exceeded")
		}
		for _, child := range d.Tasks {
			if child.ParentID == c.id && child.Info.CompletedAt == nil {
				return errors.New("localruntime: completion requires settled children")
			}
		}
		return nil
	})
}

type taskModel struct {
	stream.Model
	controller *taskController
}

func (m *taskModel) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	events, err := m.Model.Stream(ctx, opts)
	if err != nil {
		return nil, err
	}
	if events == nil {
		return nil, errors.New("localruntime: model returned nil stream")
	}
	out := make(chan stream.Event)
	go func() {
		defer close(out)
		finished := false
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if finish, ok := event.Data.(stream.FinishEvent); ok {
					var err error
					if finished {
						err = errors.New("localruntime: duplicate finish event")
					} else {
						err = m.controller.charge(ctx, finish.Usage)
						if err == nil {
							err = m.controller.update(ctx, func(_ *stateData, t *taskRecord) error {
								if t.Info.Error != "" {
									return errors.New(t.Info.Error)
								}
								return nil
							})
						}
					}
					finished = true
					if err != nil {
						event = stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{Error: err}}
					}
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

type discardEvents struct{}

func (*discardEvents) OnTextDelta(stream.TextDeltaEvent)                                    {}
func (*discardEvents) OnToolCall(stream.ToolCallEvent)                                      {}
func (*discardEvents) OnToolResult(stream.ToolResultEvent)                                  {}
func (*discardEvents) OnPermissionAsked(bus.PermissionAskedPayload)                         {}
func (*discardEvents) OnAutomaticCompactionStarted(bus.AutomaticCompactionStartedPayload)   {}
func (*discardEvents) OnAutomaticCompactionFinished(bus.AutomaticCompactionFinishedPayload) {}
func (*discardEvents) OnSuspension(*sol.SuspensionContext)                                  {}
