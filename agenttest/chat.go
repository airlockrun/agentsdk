package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/localruntime"
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
	if e == nil || e.Agent == nil || (in.Backend == nil && e.Storage == nil) {
		return nil, errors.New("agenttest: agent and platform backend are required")
	}
	backend := &chatBackend{handler: e.Agent.Handler(), scope: scope, platform: in.Backend, catalog: in.Capabilities}
	if e.Storage != nil {
		backend.platform = &storageBackend{app: backend, fallback: in.Backend}
		in.Model = &storageModel{Model: in.Model, storage: e.Storage, manifest: e.Agent.Manifest()}
	}
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
	backend := &localruntime.AppBackend{URL: "http://fixture.app", Token: "test-token", Client: &http.Client{Transport: appHandlerTransport{backend: b}}, Scope: b.scope, Catalog: b.catalog, Platform: b.platform}
	return backend.Invoke(ctx, invocation)
}

type appHandlerTransport struct{ backend *chatBackend }

func (t appHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.backend.handler == nil {
		return nil, errors.New("agenttest: app HTTP handler is required")
	}
	w := httptest.NewRecorder()
	t.backend.handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		var response wire.RuntimeInvokeResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			return nil, err
		}
		t.backend.mu.Lock()
		t.backend.responses = append(t.backend.responses, response)
		t.backend.mu.Unlock()
	}
	return w.Result(), nil
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
