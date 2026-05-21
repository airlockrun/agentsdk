package agentsdk

import (
	"context"
	"sync"

	"github.com/airlockrun/goai/tool"
	"github.com/dop251/goja"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// run is an unexported per-request bookkeeping struct. Accumulates actions
// and logs; flushed on Complete via /api/agent/run/complete. Never surfaced
// in the builder API — carried through context instead (see context.go).
type run struct {
	agent               *Agent
	id                  string
	bridgeID            string
	conversationID      string
	parentRunID         string // for A2A/external MCP calls — the caller's run ID from X-Parent-Run-ID; gates __incoming/run-<id>/ reads
	userID              string // the originating user (anchor for scoped dirs); empty for cron/webhook/anon
	supportedModalities []string
	callerAccess        Access      // resolved per-turn access level (default AccessAdmin for trusted triggers)
	visibleSiblings     []uuid.UUID // per-user sibling IDs A2A-callable on this run; intersected with PromptData.Siblings at render time
	ctx                 context.Context
	gw                  *goWall // go-call time accumulator (L3 CPU guard)
	actions             []Action
	logs                []LogEntry
	logsBytes           int // running size of logs[].Message; drives the cap in logAppend
	logger              *zap.Logger
	loggerOnce          sync.Once
	vm                  *goja.Runtime
	vmOnce              sync.Once
	mu                  sync.Mutex          // guards actions, logs, pendingLogs, attachedKeys, pendingAttachments
	attachedKeys        map[string]struct{} // keys attached this run for idempotency
	pendingLogs         []LogEntry          // logs from current executeJS call, drained after each execution
	pendingAttachments  []tool.Attachment   // attachToContext results, drained by run_js into the tool.Result
}

func newRun(agent *Agent, id, bridgeID, conversationID string, ctx context.Context) *run {
	// One go-call accumulator per run, carried in ctx so the central HTTP
	// seam credits blocking time without wrapping every binding. It feeds
	// the L3 CPU guard (JS time = wall − time parked in Go calls).
	gw := &goWall{}
	return &run{
		agent:          agent,
		id:             id,
		bridgeID:       bridgeID,
		conversationID: conversationID,
		ctx:            withGoWall(ctx, gw),
		gw:             gw,
		// Default to admin — webhook/cron/route handlers and tests are
		// trusted contexts. /prompt overrides this with the per-turn
		// CallerAccess from PromptInput.
		callerAccess: AccessAdmin,
	}
}

// checkedCtx returns r.ctx with both the run pointer and the caller
// attached. VM bindings that reach untrusted territory (storage paths,
// etc.) call this and then pass the resulting ctx to
// agent.CheckFileAccess. User-registered tools dispatched from the VM
// also receive this ctx so their bodies can call CheckFileAccess
// without losing the caller's access level. Builder Go code that calls
// the trusted file API directly (agent.OpenFile/ReadFile/...) does not
// need this — those methods skip the access check.
func (r *run) checkedCtx() context.Context {
	return WithCaller(contextWithRun(r.ctx, r), Caller{
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

// maxRunLogBytes caps the in-memory run log buffer. The buffer is a
// failure snapshot, not a log store — once over the cap, oldest entries
// are dropped. ~64 KiB comfortably holds a handler's worth of lines;
// anything chattier should be read from container stdout / the
// operator's log pipeline, where nothing is dropped.
const maxRunLogBytes = 64 * 1024

// logAppend records a run-scoped log line into the bounded buffer.
// Flushed to Airlock on Complete; persisted there only for failed runs.
// Reached from Agent.Logger's capture core and from the JS log()/console
// bindings.
func (r *run) logAppend(level LogLevel, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, LogEntry{Level: level, Message: msg})
	r.logsBytes += len(msg)
	for r.logsBytes > maxRunLogBytes && len(r.logs) > 1 {
		r.logsBytes -= len(r.logs[0].Message)
		r.logs = r.logs[1:]
	}
}

// runLogger lazily builds the per-run *zap.Logger: the shared stdout
// core tagged with run_id/agent_id, teed into a runLogCore that
// captures entries into r.logs. Built once per run; safe for
// concurrent handler goroutines.
func (r *run) runLogger() *zap.Logger {
	r.loggerOnce.Do(func() {
		base := agentLogger().Core()
		tagged := base.With([]zapcore.Field{
			zap.String("run_id", r.id),
			zap.String("agent_id", r.agent.agentID),
		})
		capture := &runLogCore{LevelEnabler: base, run: r}
		r.logger = zap.New(zapcore.NewTee(tagged, capture))
	})
	return r.logger
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
