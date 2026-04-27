package agentsdk

import (
	"context"
	"encoding/json"
	"time"
)

// recordAction appends an action to the run's action log.
func (r *run) recordAction(actionType string, request any, response any, err error, duration time.Duration) {
	a := Action{
		Type:       actionType,
		Timestamp:  time.Now(),
		DurationMs: duration.Milliseconds(),
		Request:    request,
		Response:   response,
	}
	if err != nil {
		a.Error = err.Error()
	}
	r.mu.Lock()
	r.actions = append(r.actions, a)
	r.mu.Unlock()
}

// complete flushes the run's recorded actions to Airlock. Called only by
// dispatchers (serve.go webhook/cron, prompt.go, route wrapper, background
// flusher) — never by builder code.
func (r *run) complete(ctx context.Context, status, errMsg, panicTrace string) error {
	if status == "success" && r.hasActionErrors() {
		status = "tool_errors"
	}
	return r.completeWithCheckpoint(ctx, status, errMsg, panicTrace, nil)
}

func (r *run) hasActionErrors() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.actions {
		if a.Error != "" {
			return true
		}
	}
	return false
}

func (r *run) completeWithCheckpoint(ctx context.Context, status, errMsg, panicTrace string, checkpoint json.RawMessage) error {
	body := struct {
		RunID      string          `json:"runId"`
		Status     string          `json:"status"`
		Error      string          `json:"error,omitempty"`
		PanicTrace string          `json:"panicTrace,omitempty"`
		Actions    []Action        `json:"actions"`
		Logs       []LogEntry      `json:"logs,omitempty"`
		Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	}{
		RunID:      r.id,
		Status:     status,
		Error:      errMsg,
		PanicTrace: panicTrace,
		Actions:    r.actions,
		Logs:       r.logs,
		Checkpoint: checkpoint,
	}
	if body.Actions == nil {
		body.Actions = []Action{} // always send array, not null
	}
	// Detach from the caller's ctx: final bookkeeping MUST land even if the
	// /prompt request ctx was cancelled (e.g. Airlock closed the response body
	// after seeing an error event), otherwise the run stays 'running' forever.
	// A 10s timeout still bounds the call in case Airlock is wedged.
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return r.agent.client.doJSON(detached, "POST", "/api/agent/run/complete", body, nil)
}
