package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRunComplete(t *testing.T) {
	a, mock := testAgent(t)
	run := newRun(a, "run-1", "", "", context.Background())

	run.logAppend(LogLevelInfo, "test log")
	run.recordAction("proxy", "req", "resp", nil, 100)

	err := run.complete(context.Background(), "success", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	reqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 complete request, got %d", len(reqs))
	}

	var body struct {
		RunID   string     `json:"runId"`
		Status  string     `json:"status"`
		Actions []Action   `json:"actions"`
		Logs    []LogEntry `json:"logs"`
	}
	json.Unmarshal(reqs[0].Body, &body)
	if body.RunID != "run-1" {
		t.Fatalf("expected run-1, got %s", body.RunID)
	}
	if body.Status != "success" {
		t.Fatalf("expected success, got %s", body.Status)
	}
	if len(body.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(body.Actions))
	}
	if len(body.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(body.Logs))
	}
}

// TestRunComplete_SurvivesCancelledCtx guards against the bug where Airlock
// cancelling the /prompt response body also cancels the agent's
// /api/agent/run/complete POST, leaving the run stuck as 'running'.
// completeWithCheckpoint must detach from the caller's ctx.
func TestRunComplete_SurvivesCancelledCtx(t *testing.T) {
	a, mock := testAgent(t)
	run := newRun(a, "run-cancel", "", "", context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: represents Airlock closing /prompt mid-stream

	if err := run.complete(ctx, "error", "simulated", ErrorKindAgent, ""); err != nil {
		t.Fatalf("complete with cancelled ctx should still succeed, got: %v", err)
	}

	reqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 complete request, got %d", len(reqs))
	}
}
