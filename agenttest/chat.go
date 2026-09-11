package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
)

// ChatResult includes app invocation telemetry without completing the borrowed
// run. Run is the actual Sol result, including any permission suspension.
type ChatResult struct {
	Run      *sol.RunResult
	AppCalls []wire.RuntimeInvokeResponse
}

// Chat runs the shared chat runtime locally. Input.Backend handles platform
// capabilities; app capabilities go through the real authenticated SDK handler.
// Supply mock models, a MemoryStore, an Events sink, and an executor factory.
// Input.Capabilities is an explicit subset of the manifest-based catalog; app
// declarations are checked again by the SDK endpoint on every call.
func (e *Env) Chat(ctx context.Context, scope wire.RuntimeContext, in chatruntime.Input) (*ChatResult, error) {
	if e == nil || e.Agent == nil || in.Backend == nil {
		return nil, errors.New("agenttest: agent and platform backend are required")
	}
	backend := &chatBackend{handler: e.Agent.Handler(), scope: scope, platform: in.Backend, catalog: in.Capabilities}
	in.Backend = backend
	result, err := chatruntime.Run(ctx, in)
	return &ChatResult{Run: result, AppCalls: backend.responses}, err
}

type chatBackend struct {
	handler   http.Handler
	scope     wire.RuntimeContext
	platform  chatruntime.Backend
	catalog   []capability.Definition
	mu        sync.Mutex
	responses []wire.RuntimeInvokeResponse
}

func (b *chatBackend) Invoke(ctx context.Context, invocation chatruntime.Invocation) (tool.Result, error) {
	var selected *capability.Definition
	for i := range b.catalog {
		if b.catalog[i].Path.ID() == invocation.CapabilityID {
			selected = &b.catalog[i]
			break
		}
	}
	if selected == nil {
		return tool.Result{}, errors.New("unknown test capability")
	}
	if selected.Target == capability.Platform {
		return b.platform.Invoke(ctx, invocation)
	}
	if selected.Target != capability.App {
		return tool.Result{}, errors.New("executor intrinsic cannot be dispatched")
	}
	body, err := json.Marshal(wire.RuntimeInvokeRequest{RuntimeProtocol: wire.AppRuntimeProtocol, Context: b.scope, CapabilityID: invocation.CapabilityID, ToolCallID: invocation.ToolCallID, Input: invocation.Input})
	if err != nil {
		return tool.Result{}, err
	}
	req := httptest.NewRequest(http.MethodPost, wire.RuntimeInvokePath, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	b.handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return tool.Result{}, fmt.Errorf("app invocation: HTTP %d: %s", w.Code, w.Body.String())
	}
	var response wire.RuntimeInvokeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		return tool.Result{}, err
	}
	if err := wire.CheckAppRuntimeProtocol(response.RuntimeProtocol); err != nil {
		return tool.Result{}, err
	}
	b.mu.Lock()
	b.responses = append(b.responses, response)
	b.mu.Unlock()
	result := tool.Result{Output: response.Output, Title: response.Title, Metadata: response.Metadata}
	for _, ref := range response.Attachments {
		result.Attachments = append(result.Attachments, tool.Attachment{Data: "s3ref:" + ref.Path, MimeType: ref.MimeType, Filename: ref.Filename})
	}
	if response.Error != "" {
		return result, errors.New(response.Error)
	}
	return result, nil
}

// MemoryStore is a test-only conversation store. Production hosts use shared
// persistence and serialize runs for each conversation across replicas.
type MemoryStore struct {
	mu       sync.Mutex
	messages []session.Message
}

func (s *MemoryStore) Load(ctx context.Context) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s.messages)
	if err != nil {
		return nil, err
	}
	var messages []session.Message
	err = json.Unmarshal(b, &messages)
	return messages, err
}

func (s *MemoryStore) Append(ctx context.Context, messages []session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	var copy []session.Message
	if err := json.Unmarshal(b, &copy); err != nil {
		return err
	}
	s.messages = append(s.messages, copy...)
	return nil
}

func (s *MemoryStore) Compact(ctx context.Context, summary []session.Message, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	var copy []session.Message
	if err := json.Unmarshal(b, &copy); err != nil {
		return err
	}
	s.messages = copy
	return nil
}

// Events records typed eventstream.Sink payloads for assertions after Run.
type Events struct {
	mu     sync.Mutex
	events []any
}

func (e *Events) record(v any) { e.mu.Lock(); defer e.mu.Unlock(); e.events = append(e.events, v) }
func (e *Events) Snapshot() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]any(nil), e.events...)
}
func (e *Events) OnTextDelta(v stream.TextDeltaEvent)                                    { e.record(v) }
func (e *Events) OnToolCall(v stream.ToolCallEvent)                                      { e.record(v) }
func (e *Events) OnToolResult(v stream.ToolResultEvent)                                  { e.record(v) }
func (e *Events) OnPermissionAsked(v bus.PermissionAskedPayload)                         { e.record(v) }
func (e *Events) OnAutomaticCompactionStarted(v bus.AutomaticCompactionStartedPayload)   { e.record(v) }
func (e *Events) OnAutomaticCompactionFinished(v bus.AutomaticCompactionFinishedPayload) { e.record(v) }
func (e *Events) OnSuspension(v *sol.SuspensionContext)                                  { e.record(v) }
