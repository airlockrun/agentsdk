package agentsdk

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestLoggerCapturesIntoRun(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "run-log-1", "", "", context.Background())
	ctx := contextWithRun(context.Background(), run)

	log := a.Logger(ctx)
	log.Info("imported rows", zap.Int("count", 42))
	log.Warn("retrying")
	log.Error("gave up", zap.String("reason", "timeout"))

	if len(run.logs) != 3 {
		t.Fatalf("expected 3 captured entries, got %d: %v", len(run.logs), run.logs)
	}
	// Level mapping.
	if run.logs[0].Level != LogLevelInfo || run.logs[1].Level != LogLevelWarn || run.logs[2].Level != LogLevelError {
		t.Fatalf("level mapping wrong: %v", run.logs)
	}
	// Structured fields flatten into the captured message.
	if !strings.Contains(run.logs[0].Message, "imported rows") || !strings.Contains(run.logs[0].Message, "count=42") {
		t.Fatalf("expected fields flattened, got %q", run.logs[0].Message)
	}
	if !strings.Contains(run.logs[2].Message, "reason=timeout") {
		t.Fatalf("expected reason field, got %q", run.logs[2].Message)
	}
}

func TestLoggerNoRunDoesNotPanic(t *testing.T) {
	a, _ := testAgent(t)
	// No run bound to ctx — must fall back to the base logger, not panic.
	log := a.Logger(context.Background())
	if log == nil {
		t.Fatal("expected a non-nil base logger")
	}
	log.Info("startup line") // exercises the base logger path
}

func TestLogAppendCap(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "run-cap-1", "", "", context.Background())

	// One ~1 KiB line; enough of them to blow well past the 64 KiB cap.
	line := strings.Repeat("x", 1024)
	for i := 0; i < 200; i++ {
		run.logAppend(LogLevelInfo, line)
	}

	if run.logsBytes > maxRunLogBytes {
		t.Fatalf("logsBytes %d exceeds cap %d", run.logsBytes, maxRunLogBytes)
	}
	// The buffer kept the most recent entries and dropped the oldest.
	total := 0
	for _, e := range run.logs {
		total += len(e.Message)
	}
	if total != run.logsBytes {
		t.Fatalf("logsBytes %d disagrees with summed messages %d", run.logsBytes, total)
	}
	if len(run.logs) == 200 {
		t.Fatal("expected oldest entries to be dropped")
	}
}
