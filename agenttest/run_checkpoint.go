package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/session"
)

// runCheckpointStore is attempt-local test persistence. Every boundary copies
// JSON, including the revision retry payload, to avoid relying on heap aliasing.
type runCheckpointStore struct {
	mu         sync.Mutex
	messages   []session.Message
	checkpoint json.RawMessage
}

func runClone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var copy T
	if err := json.Unmarshal(raw, &copy); err != nil {
		panic(err)
	}
	return copy
}

func (s *runCheckpointStore) Load(ctx context.Context) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return runClone(s.messages), nil
}

func (*runCheckpointStore) Append(context.Context, []session.Message) error {
	return errors.New("agenttest: task messages require atomic SaveCheckpoint")
}

func (s *runCheckpointStore) Compact(ctx context.Context, messages []session.Message, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.messages = runClone(messages)
	return nil
}

func (s *runCheckpointStore) LoadCheckpoint(ctx context.Context) (*agentruntime.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.loadCheckpoint()
}

func (s *runCheckpointStore) loadCheckpoint() (*agentruntime.Checkpoint, error) {
	if s.checkpoint == nil {
		return nil, nil
	}
	var cp agentruntime.Checkpoint
	if err := json.Unmarshal(s.checkpoint, &cp); err != nil {
		return nil, err
	}
	// Append is a committed transaction payload, not pending work.
	cp.Append = nil
	return &cp, nil
}

func (s *runCheckpointStore) SaveCheckpoint(ctx context.Context, cp *agentruntime.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if cp == nil {
		return errors.New("agenttest: checkpoint is required")
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	previous, err := s.loadCheckpoint()
	if err != nil {
		return err
	}
	if previous != nil && previous.Revision == cp.Revision && bytes.Equal(raw, s.checkpoint) {
		return nil
	}
	expected := int64(1)
	if previous != nil {
		expected = previous.Revision + 1
	}
	if cp.Revision != expected {
		return errors.New("agenttest: stale checkpoint revision")
	}
	s.messages = append(s.messages, runClone(cp.Append)...)
	s.checkpoint = append(json.RawMessage(nil), raw...)
	return nil
}

func (s *runCheckpointStore) snapshot() ([]session.Message, *agentruntime.Checkpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.loadCheckpoint()
	if err != nil {
		panic(err)
	}
	return runClone(s.messages), cp
}

type leafRunController struct {
	steps, maxSteps, maxTokens int64
	mu                         sync.Mutex
	tokens                     int64
}

// Provider accounting is independent of transcript replacement and checkpoints:
// compaction responses incur usage even though their message tokens are cleared.
func (c *leafRunController) chargeUsage(usage stream.Usage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tokens := int64(usage.GrandTotal())
	if tokens > math.MaxInt64-c.tokens {
		c.tokens = math.MaxInt64
	} else {
		c.tokens += tokens
	}
	if c.maxTokens > 0 && (usage.InputTokens.Total == nil || usage.OutputTokens.Total == nil) {
		return errors.New("agenttest: token budget requires reported input and output usage")
	}
	if c.maxTokens > 0 && c.tokens >= c.maxTokens {
		return errors.New("agenttest: token budget exceeded")
	}
	return nil
}

func (c *leafRunController) tokenCount() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.tokens }

func (c *leafRunController) BeforeModel(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.steps >= c.maxSteps {
		return errors.New("agenttest: step budget exceeded")
	}
	if err := c.checkTokens(ctx); err != nil {
		return err
	}
	c.steps++
	return nil
}

func (c *leafRunController) checkTokens(ctx context.Context) error {
	if c.maxTokens == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tokens := c.tokenCount()
	if tokens >= c.maxTokens {
		return errors.New("agenttest: token budget exceeded")
	}
	return nil
}

func (c *leafRunController) Complete(ctx context.Context, _ wire.AgentReply) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.checkTokens(ctx)
}

var errRunChildren = errors.New("agenttest: child controls are unsupported for leaf execution")

func (*leafRunController) Spawn(context.Context, string, string, json.RawMessage) (wire.AgentRunInfo, error) {
	return wire.AgentRunInfo{}, errRunChildren
}
func (*leafRunController) Continue(context.Context, string, string, string) (wire.AgentRunInfo, error) {
	return wire.AgentRunInfo{}, errRunChildren
}
func (*leafRunController) Get(context.Context, []string) ([]wire.AgentRunInfo, error) {
	return nil, errRunChildren
}
func (*leafRunController) Wait(context.Context, string, wire.AgentWaitRequest) (wire.AgentWaitResult, error) {
	return wire.AgentWaitResult{}, errRunChildren
}
func (*leafRunController) Cancel(context.Context, []string) (wire.AgentCancelResult, error) {
	return wire.AgentCancelResult{}, errRunChildren
}

// runBudgetModel observes every runtime model request, including compaction,
// before its finish event can release any private tool or completion dispatch.
type runBudgetModel struct {
	stream.Model
	controller *leafRunController
	workers    sync.WaitGroup
}

func (m *runBudgetModel) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	events, err := m.Model.Stream(ctx, opts)
	if err != nil {
		return nil, err
	}
	if events == nil {
		return nil, errors.New("agenttest: model returned a nil stream")
	}
	out := make(chan stream.Event)
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
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
						err = errors.New("agenttest: model returned duplicate finish events")
					} else {
						err = m.controller.chargeUsage(finish.Usage)
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
