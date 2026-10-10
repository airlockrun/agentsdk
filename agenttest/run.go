package agenttest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

// RunOptions supplies the host boundaries for an explicit registered-task test.
// A zero Scope creates a fresh application-owned test run using Env's app ID.
// An explicit Scope must identify the same app and an application-owned run;
// Definition is filled from the handle and must match if supplied.
type RunOptions struct {
	Scope           wire.RuntimeContext
	ExecutorFactory chatruntime.ExecutorFactory
	// Backend handles every non-private capability, including app storage and DB.
	// Without it these calls fail with an unsupported-service error.
	Backend   chatruntime.Backend
	Discovery capability.Discovery
	// Timeout bounds the entire attempt (default one minute). MaxSteps bounds
	// model requests (default 150). Registered limits can further restrict both.
	Timeout  time.Duration
	MaxSteps int64
}

// RunResult retains diagnostics on failed attempts as well as typed completion.
type RunResult[Out any] struct {
	Reply      *agentsdk.AgentReply[Out]
	Runtime    *agentruntime.Result
	Scope      wire.RuntimeContext
	Steps      int64
	Tokens     int64
	Calls      []chatruntime.Invocation
	AppCalls   []wire.RuntimeInvokeResponse
	Events     []any
	Messages   []session.Message
	Checkpoint *agentruntime.Checkpoint
}

// RunAgent executes a leaf registered task through agentruntime and the supplied
// Deno executor. Models are selected by the environment's registered slot policy.
// This diagnostic leaf helper uses attempt-local fixtures. Native Start/Get/Wait
// exercise Env.Runtime's durable scheduler, including declared child agents.
func RunAgent[In, Out any](t *testing.T, ctx context.Context, env *Env, handle *agentsdk.AgentHandle[In, Out], input In, opts RunOptions) (*RunResult[Out], error) {
	t.Helper()
	if ctx == nil || env == nil || env.Agent == nil || handle == nil {
		return nil, errors.New("agenttest: context, environment and registered handle are required")
	}
	if opts.ExecutorFactory == nil {
		return nil, errors.New("agenttest: ExecutorFactory is required")
	}
	if opts.Timeout < 0 || opts.MaxSteps < 0 {
		return nil, errors.New("agenttest: timeout and steps must be nonnegative")
	}
	manifest := env.Agent.Manifest()
	var def *wire.AgentDefinition
	for i := range manifest.AgentDefinitions {
		d := &manifest.AgentDefinitions[i]
		if d.Slug == handle.Slug() && d.ContractHash == handle.ContractHash() {
			def = d
			break
		}
	}
	if def == nil {
		return nil, errors.New("agenttest: handle contract is not registered in this environment")
	}
	if len(def.Subagents) != 0 {
		return nil, errors.New("agenttest: RunAgent supports leaf definitions only; children are unsupported")
	}
	for _, slug := range def.MCPs {
		if _, ok := opts.Discovery.MCPSchemas[slug]; !ok || opts.Backend == nil {
			return nil, fmt.Errorf("agenttest: MCP %q requires explicit discovery schemas and Backend", slug)
		}
	}
	scope := opts.Scope
	if reflect.DeepEqual(scope, wire.RuntimeContext{}) {
		if env.appID == "" {
			return nil, errors.New("agenttest: environment app identity is required for generated Scope")
		}
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, fmt.Errorf("agenttest: generate invocation token: %w", err)
		}
		scope = wire.RuntimeContext{AgentID: env.appID, RunID: uuid.NewString(), InvocationToken: hex.EncodeToString(token[:]),
			Caller: wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: "background"}}}
	}
	identity := wire.RuntimeAgentDefinition{Slug: def.Slug, ContractHash: def.ContractHash}
	if scope.Definition != nil && *scope.Definition != identity {
		return nil, errors.New("agenttest: scope definition contract mismatch")
	}
	scope.Definition = &identity
	if scope.AgentID == "" || scope.RunID == "" || scope.InvocationToken == "" {
		return nil, errors.New("agenttest: Scope requires agentId, runId and invocationToken")
	}
	appID, err := uuid.Parse(scope.AgentID)
	if err != nil || appID.String() != scope.AgentID {
		return nil, errors.New("agenttest: invalid scope app ID")
	}
	if env.appID == "" || scope.AgentID != env.appID {
		return nil, errors.New("agenttest: scope app identity mismatch")
	}
	for _, id := range []string{scope.RunID, scope.ConversationID, scope.BridgeID} {
		if id == "" {
			continue
		}
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id {
			return nil, errors.New("agenttest: invalid scope run/conversation/bridge ID")
		}
	}
	if token, err := hex.DecodeString(scope.InvocationToken); err != nil || len(token) != 32 || hex.EncodeToString(token) != scope.InvocationToken {
		return nil, errors.New("agenttest: invalid scope invocation token")
	}
	if err := scope.Caller.Validate(); err != nil {
		return nil, fmt.Errorf("agenttest: scope caller: %w", err)
	}
	if scope.Caller.Kind != "application" || scope.Caller.Access != wire.AccessAdmin || scope.Caller.Initiator != nil || scope.Job != nil {
		return nil, errors.New("agenttest: scope requires an application-owned admin caller without a job fence")
	}
	catalog, err := capability.DefinitionCatalog(manifest, identity, opts.Discovery)
	if err != nil {
		return nil, err
	}
	// Match the registered-task host policy: tasks have no conversation output
	// delivery or app-upgrade capability, even with an explicitly supplied backend.
	allowed := catalog[:0]
	for _, d := range catalog {
		if d.Path.Kind() == capability.Air && (d.Path.CanonicalOperation() == "output" || d.Path.CanonicalOperation() == "request_upgrade") {
			continue
		}
		allowed = append(allowed, d)
	}
	catalog = allowed
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("agenttest: encode input: %w", err)
	}
	if err := agentruntime.ValidateJSON(def.InputSchema, raw); err != nil {
		return nil, fmt.Errorf("agenttest: input contract: %w", err)
	}
	model, limits, err := env.modelForSlot(def.ModelSlot)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	if def.Budget.TimeoutMS > 0 && time.Duration(def.Budget.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(def.Budget.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(t.Context(), cancel)
	t.Cleanup(cancel)
	defer cancel()
	defer stop()
	steps := opts.MaxSteps
	if steps == 0 {
		steps = 150
	}
	if def.Budget.Steps > 0 && def.Budget.Steps < steps {
		steps = def.Budget.Steps
	}
	store := &runCheckpointStore{}
	controller := &leafRunController{maxSteps: steps, maxTokens: def.Budget.Tokens}
	budgetModel := &runBudgetModel{Model: model, controller: controller}
	events := &Events{}
	app := &chatBackend{scope: scope, catalog: catalog}
	if len(def.Tools) > 0 || env.Storage != nil {
		app.handler = env.Agent.Handler()
	}
	backend := &registeredRunBackend{app: app, platform: opts.Backend, private: map[string]bool{}}
	if env.Storage != nil {
		backend.platform = &storageBackend{app: app, fallback: opts.Backend}
		for _, d := range catalog {
			if d.Target == capability.App {
				backend.private[d.Path.ID()] = true
			}
		}
	}
	for _, private := range def.Tools {
		backend.private[capability.Local(capability.Tool, "", private.Name).ID()] = true
	}
	runtimeResult, runErr := agentruntime.Run(ctx, agentruntime.Input{
		Definition: *def, Message: string(raw), Model: budgetModel, ModelLimits: limits,
		Store: store, Controller: controller, Sink: events, Backend: backend,
		Capabilities: catalog, ExecutorFactory: opts.ExecutorFactory,
		Redactor: func(s string) string {
			for _, secret := range []string{"test-token", scope.InvocationToken} {
				if secret != "" {
					s = strings.ReplaceAll(s, secret, "[REDACTED]")
				}
			}
			return s
		},
	})
	budgetModel.workers.Wait()
	result := &RunResult[Out]{Runtime: runtimeResult, Scope: scope, Steps: controller.steps, Tokens: controller.tokenCount(), Events: events.Snapshot()}
	result.Messages, result.Checkpoint = store.snapshot()
	backend.mu.Lock()
	result.Calls = append([]chatruntime.Invocation(nil), backend.calls...)
	backend.mu.Unlock()
	app.mu.Lock()
	result.AppCalls = append([]wire.RuntimeInvokeResponse(nil), app.responses...)
	app.mu.Unlock()
	if runErr != nil {
		return result, runErr
	}
	if runtimeResult == nil || runtimeResult.Reply == nil {
		return result, errors.New("agenttest: runtime ended without completion")
	}
	reply := runtimeResult.Reply
	typed := &agentsdk.AgentReply[Out]{Kind: agentsdk.AgentReplyKind(reply.Kind), Question: reply.Question}
	if reply.Kind == "output" {
		if err := agentruntime.ValidateJSON(def.OutputSchema, reply.Output); err != nil {
			return result, fmt.Errorf("agenttest: output contract: %w", err)
		}
		var output Out
		decoder := json.NewDecoder(bytes.NewReader(reply.Output))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&output); err != nil {
			return result, fmt.Errorf("agenttest: typed output: %w", err)
		}
		typed.Output = &output
	}
	result.Reply = typed
	return result, nil
}

type registeredRunBackend struct {
	app      *chatBackend
	platform chatruntime.Backend
	private  map[string]bool
	mu       sync.Mutex
	calls    []chatruntime.Invocation
}

func (b *registeredRunBackend) Invoke(ctx context.Context, invocation chatruntime.Invocation) (tool.Result, error) {
	b.mu.Lock()
	copy := invocation
	copy.Input = append(json.RawMessage(nil), invocation.Input...)
	b.calls = append(b.calls, copy)
	b.mu.Unlock()
	if b.private[invocation.CapabilityID] {
		return b.app.Invoke(ctx, invocation)
	}
	if b.platform == nil {
		return tool.Result{}, fmt.Errorf("agenttest: unsupported service %q; supply Backend", invocation.CapabilityID)
	}
	return b.platform.Invoke(ctx, invocation)
}
