package agentsdk

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logging model
// =============
//
// agentsdk has one logger. baseLogger writes structured JSON to stdout —
// uniform with airlock's own zap output, so an enterprise log pipeline
// scrapes the container and gets everything. Framework-internal lines
// (serve/sync/migrate/...) log straight to it.
//
// Handler code gets a per-run logger via Agent.Logger(ctx): the same
// stdout core, tagged with run_id/agent_id, teed into a per-run capture
// core (runLogCore) that accumulates entries in the run's bounded
// buffer. That buffer is posted to airlock on completion and stored as
// the run's log record (rendered in the run-detail UI; a failed run's
// copy also feeds the Fix-this-error builder). airlock ages it out
// with the run's other verbose fields at compaction.

var (
	baseLoggerOnce sync.Once
	baseLogger     *zap.Logger
)

// agentLogger returns the process-wide base logger: JSON to stdout,
// level from AIRLOCK_LOG_LEVEL (default info). Built once.
func agentLogger() *zap.Logger {
	baseLoggerOnce.Do(func() {
		level := zapcore.InfoLevel
		if s := os.Getenv("AIRLOCK_LOG_LEVEL"); s != "" {
			_ = level.UnmarshalText([]byte(s))
		}
		cfg := zap.NewProductionConfig()
		cfg.Level = zap.NewAtomicLevelAt(level)
		l, err := cfg.Build()
		if err != nil {
			l = zap.NewNop()
		}
		baseLogger = l
	})
	return baseLogger
}

// zapLevelToLogLevel collapses a zap level onto the three-value LogLevel
// the wire format carries. Levels above error (DPanic/Panic/Fatal) fold
// into error — the run buffer is a diagnostic snapshot, not a faithful
// level ladder.
func zapLevelToLogLevel(l zapcore.Level) LogLevel {
	switch {
	case l >= zapcore.ErrorLevel:
		return LogLevelError
	case l == zapcore.WarnLevel:
		return LogLevelWarn
	case l <= zapcore.DebugLevel:
		return LogLevelDebug
	default:
		return LogLevelInfo
	}
}

// runLogCore is a zapcore.Core that captures entries into a run's
// bounded buffer instead of writing anywhere. It is teed alongside the
// stdout core so one Logger call lands in both places. Structured
// fields are flattened into the message ` key=value` so the failure
// snapshot keeps the diagnostic detail an LLM needs; full-fidelity
// fields stay on the JSON stdout line.
type runLogCore struct {
	zapcore.LevelEnabler
	run    *run
	fields []zapcore.Field // accumulated via With()
}

func (c *runLogCore) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	merged = append(merged, c.fields...)
	merged = append(merged, fields...)
	return &runLogCore{LevelEnabler: c.LevelEnabler, run: c.run, fields: merged}
}

func (c *runLogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *runLogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	msg := e.Message

	all := fields
	if len(c.fields) > 0 {
		all = append(append([]zapcore.Field{}, c.fields...), fields...)
	}
	if len(all) > 0 {
		// A fresh MapObjectEncoder per call — encoders are not
		// concurrency-safe, and runLogCore.Write can be reached from
		// builder goroutines.
		enc := zapcore.NewMapObjectEncoder()
		for _, f := range all {
			f.AddTo(enc)
		}
		if len(enc.Fields) > 0 {
			keys := make([]string, 0, len(enc.Fields))
			for k := range enc.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			b.WriteString(msg)
			for _, k := range keys {
				fmt.Fprintf(&b, " %s=%v", k, enc.Fields[k])
			}
			msg = b.String()
		}
	}

	c.run.logAppend(zapLevelToLogLevel(e.Level), msg)
	return nil
}

func (c *runLogCore) Sync() error { return nil }
