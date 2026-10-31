package agentsdk

import (
	"context"
	"encoding/json"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

// recordAction appends an action to the run's action log.
func (r *run) recordAction(actionType string, request any, response any, err error, duration time.Duration) {
	a := wire.Action{
		Type:       actionType,
		Timestamp:  time.Now(),
		DurationMs: duration.Milliseconds(),
		Request:    request,
		Response:   response,
	}
	if err != nil {
		a.Error = wire.BoundedAuditText(err.Error(), wire.MaxAuditErrorBytes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.actionErrors = true
	}
	if r.loggingOff || len(r.actions) >= wire.MaxAuditActions {
		return
	}
	bounded := wire.BoundAuditActions([]wire.Action{a})
	if len(bounded) == 0 {
		return
	}
	a = bounded[0]
	encoded, _ := json.Marshal(a)
	if r.actionsBytes+len(encoded) > wire.MaxAuditActionsBytes {
		return
	}
	r.actionsBytes += len(encoded)
	r.actions = append(r.actions, a)
}

// complete flushes the run's recorded actions to Airlock. Called only by
// dispatchers (webhooks, jobs, prompts, routes, and background
// flusher) — never by builder code. errorKind is one of errorKindPlatform /
// errorKindAgent / "" — required when status == "error", ignored otherwise.
func (r *run) complete(ctx context.Context, status, errMsg, errorKind, panicTrace string) error {
	if status == "success" && r.hasActionErrors() {
		status = "tool_errors"
	}
	return r.completeWithCheckpoint(ctx, status, errMsg, errorKind, panicTrace, nil)
}

func (r *run) hasActionErrors() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.actionErrors {
		return true
	}
	for _, a := range r.actions {
		if a.Error != "" {
			return true
		}
	}
	return false
}

func (r *run) completeWithCheckpoint(ctx context.Context, status, errMsg, errorKind, panicTrace string, checkpoint json.RawMessage) error {
	// The run is terminal here (every dispatcher path funnels through this
	// finalizer); drop the local-disk read cache before flushing.
	r.cleanupScratch()
	r.mu.Lock()
	actions, logs := wire.BoundAuditActions(r.actions), wire.BoundAuditLogs(r.logs)
	r.mu.Unlock()
	if r.loggingOff {
		actions, logs, errMsg, panicTrace = nil, nil, "", ""
	}
	body := wire.RunCompleteRequest{
		RunID:      r.id,
		Status:     status,
		Error:      wire.BoundedAuditText(errMsg, wire.MaxAuditErrorBytes),
		ErrorKind:  errorKind,
		PanicTrace: wire.BoundedAuditText(panicTrace, wire.MaxAuditPanicBytes),
		Actions:    actions,
		Logs:       logs,
		Checkpoint: checkpoint,
	}
	if body.Actions == nil {
		body.Actions = []wire.Action{} // always send array, not null
	}
	if job := jobRunFromContext(ctx); job != nil {
		body.JobID = job.id
		body.Attempt = int32(job.attempt)
		body.LeaseToken = job.leaseToken
	}
	// Detach from the caller's ctx: final bookkeeping MUST land even if the
	// /prompt request ctx was cancelled (e.g. Airlock closed the response body
	// after seeing an error event), otherwise the run stays 'running' forever.
	// A 10s timeout still bounds the call in case Airlock is wedged.
	detached, cancel := context.WithTimeout(context.WithoutCancel(contextWithRun(ctx, r)), 10*time.Second)
	defer cancel()
	return r.agent.client.doJSON(detached, "POST", "/api/agent/run/complete", body, nil)
}
