package agentsdk

import (
	"context"
	"sync"

	"github.com/airlockrun/goai/tool"
	"github.com/dop251/goja"
)

// run is an unexported per-request bookkeeping struct. Accumulates actions
// and logs; flushed on Complete via /api/agent/run/complete. Never surfaced
// in the builder API — carried through context instead (see context.go).
type run struct {
	agent               *Agent
	id                  string
	bridgeID            string
	conversationID      string
	supportedModalities []string
	callerAccess        Access // resolved per-turn access level (default AccessAdmin for trusted triggers)
	ctx                 context.Context
	actions             []Action
	logs                []LogEntry
	vm                  *goja.Runtime
	vmOnce              sync.Once
	mu                  sync.Mutex // guards actions, logs, pendingLogs, attachedKeys, pendingAttachments
	convVM              *ConversationVM
	attachedKeys        map[string]struct{} // keys attached this run for idempotency
	pendingLogs         []LogEntry          // logs from current executeJS call, drained after each execution
	pendingAttachments  []tool.Attachment   // attachToContext results, drained by run_js into the tool.Result
}

func newRun(agent *Agent, id, bridgeID, conversationID string, ctx context.Context) *run {
	return &run{
		agent:          agent,
		id:             id,
		bridgeID:       bridgeID,
		conversationID: conversationID,
		ctx:            ctx,
		// Default to admin — webhook/cron/route handlers and tests are
		// trusted contexts. /prompt overrides this with the per-turn
		// CallerAccess from PromptInput.
		callerAccess: AccessAdmin,
	}
}

// checkedCtx returns r.ctx with this run's caller attached. VM bindings
// that reach untrusted territory (storage paths, etc.) call this and
// then pass the resulting ctx to agent.CheckFileAccess. Builder Go code
// that calls the trusted file API directly (agent.OpenFile/ReadFile/...)
// does not need this — those methods skip the access check.
func (r *run) checkedCtx() context.Context {
	return WithCaller(r.ctx, Caller{
		Access: r.callerAccess,
		RunID:  r.id,
	})
}

// vmRuntime lazily builds the per-run goja VM. Called only from inside
// run_js tool execution — builder code never touches it.
func (r *run) vmRuntime() *goja.Runtime {
	r.vmOnce.Do(func() {
		r.vm = newVM(r, r.agent)
	})
	return r.vm
}

// logAppend records a run-scoped log line. Flushed to Airlock on Complete.
func (r *run) logAppend(level LogLevel, msg string) {
	r.mu.Lock()
	r.logs = append(r.logs, LogEntry{Level: level, Message: msg})
	r.mu.Unlock()
}

// --- VM-only Airlock calls (only reachable from run_js JS bindings) ---

// printToUser sends display parts to the run's bound conversation. If topic
// is empty, delivers to the conversation directly; if set, Airlock routes
// to all subscribed conversations (topic publish).
func (r *run) printToUser(ctx context.Context, parts []DisplayPart, topic string) error {
	for i := range parts {
		ResolveDisplayPart(&parts[i])
	}
	req := PrintRequest{
		Parts:          parts,
		Topic:          topic,
		ConversationID: r.conversationID,
		RunID:          r.id,
	}
	return r.agent.client.doJSON(ctx, "POST", "/api/agent/print", req, nil)
}

func (r *run) subscribeTopic(ctx context.Context, slug string) error {
	body := struct {
		ConversationID string `json:"conversationId"`
	}{r.conversationID}
	return r.agent.client.doJSON(ctx, "POST", "/api/agent/topic/"+slug+"/subscribe", body, nil)
}

func (r *run) unsubscribeTopic(ctx context.Context, slug string) error {
	body := struct {
		ConversationID string `json:"conversationId"`
	}{r.conversationID}
	return r.agent.client.doJSON(ctx, "DELETE", "/api/agent/topic/"+slug+"/subscribe", body, nil)
}
