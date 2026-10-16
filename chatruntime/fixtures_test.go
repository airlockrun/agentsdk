package chatruntime_test

import (
	"context"
	"encoding/json"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
	"sync"
)

// Algorithm fixtures contain only in-memory transcript/events for one unit test.
// They do not emulate host admission, jobs, durable leases or app execution.
type memoryStore struct {
	mu       sync.Mutex
	messages []session.Message
}

func cloneMessages(messages []session.Message) ([]session.Message, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	var copy []session.Message
	err = json.Unmarshal(raw, &copy)
	return copy, err
}
func (s *memoryStore) Load(ctx context.Context) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cloneMessages(s.messages)
}
func (s *memoryStore) Append(ctx context.Context, messages []session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	copy, err := cloneMessages(messages)
	if err == nil {
		s.messages = append(s.messages, copy...)
	}
	return err
}
func (s *memoryStore) Compact(ctx context.Context, messages []session.Message, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	copy, err := cloneMessages(messages)
	if err == nil {
		s.messages = copy
	}
	return err
}

type events struct {
	mu      sync.Mutex
	entries []any
}

func (e *events) record(v any) { e.mu.Lock(); defer e.mu.Unlock(); e.entries = append(e.entries, v) }
func (e *events) Snapshot() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]any(nil), e.entries...)
}
func (e *events) OnTextDelta(v stream.TextDeltaEvent)                                    { e.record(v) }
func (e *events) OnToolCall(v stream.ToolCallEvent)                                      { e.record(v) }
func (e *events) OnToolResult(v stream.ToolResultEvent)                                  { e.record(v) }
func (e *events) OnPermissionAsked(v bus.PermissionAskedPayload)                         { e.record(v) }
func (e *events) OnAutomaticCompactionStarted(v bus.AutomaticCompactionStartedPayload)   { e.record(v) }
func (e *events) OnAutomaticCompactionFinished(v bus.AutomaticCompactionFinishedPayload) { e.record(v) }
func (e *events) OnSuspension(v *sol.SuspensionContext)                                  { e.record(v) }
