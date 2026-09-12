package agentsdk

import (
	"context"
	"sync"

	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// run is an unexported per-request bookkeeping struct. Accumulates actions
// and logs; flushed on Complete via /api/agent/run/complete. Never surfaced
// in the builder API — carried through context instead (see context.go).
type run struct {
	agent           *Agent
	id              string
	invocationToken string
	bridgeID        string
	conversationID  string
	caller          Caller
	userID          string // the originating user (anchor for scoped dirs); empty for system jobs, webhooks, and anonymous runs
	callerAccess    Access // private file-access projection
	ctx             context.Context
	actions         []wire.Action
	logs            []wire.LogEntry
	logsBytes       int // running size of logs[].Message; drives the cap in logAppend
	logger          *zap.Logger
	loggerOnce      sync.Once
	mu              sync.Mutex // guards actions and logs
	fileCache       *fileCache // per-run local-disk read cache (large-file reads spill here)
	cleanupOnce     sync.Once  // guards cleanupScratch so run.complete can call it on every path
}

func newRun(agent *Agent, id, bridgeID, conversationID string, ctx context.Context) *run {
	return &run{
		agent:          agent,
		id:             id,
		bridgeID:       bridgeID,
		conversationID: conversationID,
		ctx:            ctx,
		fileCache:      newFileCache(),
	}
}

// checkedCtx attaches the run and caller for capability access checks and
// registered tool attribution. Trusted Go file APIs retain their own policy.
func (r *run) checkedCtx() context.Context {
	return withCallScope(contextWithRun(r.ctx, r), callScope{
		Access: r.callerAccess,
		UserID: r.userID,
		RunID:  r.id,
	})
}

func (r *run) setCaller(c Caller) {
	c.requireValid()
	r.caller = c
	r.callerAccess = c.Access()
	user, _ := c.User()
	r.userID = user.ID
}

// maxRunLogBytes caps the in-memory run log buffer. Airlock keeps the
// flushed buffer as the run's log record, so it must stay bounded —
// once over the cap, oldest entries are dropped. ~64 KiB comfortably
// holds a handler's worth of lines; anything chattier should be read
// from container stdout / the operator's log pipeline, where nothing
// is dropped.
const maxRunLogBytes = 64 * 1024

// logAppend records a run-scoped log line into the bounded buffer.
// Flushed to Airlock on Complete as the run's log record. Reached from
// Agent.Logger's capture core and from the JS log()/console bindings.
func (r *run) logAppend(level wire.LogLevel, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, wire.LogEntry{Level: level, Message: msg})
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

// output sends display parts to the run's bound conversation. If topic
// is empty, delivers to the conversation directly; if set, Airlock routes
// to all subscribed conversations (topic publish). Backs the `output()`
// capability and TopicHandle.Publish (Go, may include text).
func (r *run) output(ctx context.Context, parts []DisplayPart, topic string) error {
	for i := range parts {
		resolveDisplayPart(&parts[i])
		if parts[i].Source != "" {
			resolved, err := r.agent.resolveFilePath(r.checkedCtx(), parts[i].Source, FileOperationRead)
			if err != nil {
				return err
			}
			parts[i].Source = resolved
		}
	}
	req := wire.PrintRequest{
		Parts:          toWireDisplayParts(parts),
		Topic:          topic,
		ConversationID: r.conversationID,
		RunID:          r.id,
	}
	return r.agent.client.doJSON(contextWithRun(ctx, r), "POST", "/api/agent/print", req, nil)
}
