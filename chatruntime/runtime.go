// Package chatruntime runs hosted chat using explicit models, persistence,
// capability dispatch, and an isolated JavaScript executor. It has no dependency
// on the SDK root or an Airlock server.
package chatruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/agent"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/eventstream"
	"github.com/airlockrun/sol/session"
)

// Invocation carries canonical identity and the enclosing model tool call ID.
// Backend implementations bind authenticated run attribution independently of
// the script, authorize each call, and return redacted output and references.
type Invocation struct {
	CapabilityID string
	ToolCallID   string
	Input        json.RawMessage
}

type Backend interface {
	Invoke(context.Context, Invocation) (tool.Result, error)
}

// ExecutorFactory allocates a fresh realm with exactly these authorized bindings.
// Each binding takes one JSON object argument matching Definition.InputSchema.
// The executor calls Invoker with Definition.Path.ID() and a positional array.
// Run calls the factory only after approval and closes the session on every exit.
type ExecutorFactory func(context.Context, []capability.Definition) (jsexec.Session, error)

// Input is scoped to one run. The host owns authorization, run completion,
// checkpoint persistence, and conversation serialization across replicas.
// Capabilities must be the broker-authorized subset of capability.Catalog.
type Input struct {
	Message         string
	Instructions    string
	Model           stream.Model
	ModelLimits     session.ModelLimits
	SessionStore    session.SessionStore
	Capabilities    []capability.Definition
	Backend         Backend
	ExecutorFactory ExecutorFactory
	Sink            eventstream.Sink
	MaxSteps        int
	Temperature     *float64
	DirectTools     bool
	AutoConfirm     bool
	Redactor        func(string) string
	ForceCompact    bool
	// Resume accepts only a permission checkpoint produced by this runtime.
	// Approval is required when Resume is set; false explicitly denies the call.
	Resume   *sol.SuspensionContext
	Approved *bool
}

// Run executes serial model tool calls. Async capability callbacks within a
// script are bounded by the supplied jsexec implementation. Session state lives
// only for this uninterrupted run; suspension or completion destroys the realm.
func Run(ctx context.Context, in Input) (result *sol.RunResult, err error) {
	if in.Model == nil || in.SessionStore == nil || in.Backend == nil || in.Sink == nil {
		return nil, errors.New("chatruntime: Model, SessionStore, Backend and Sink are required")
	}
	if in.MaxSteps <= 0 || in.ModelLimits == (session.ModelLimits{}) {
		return nil, errors.New("chatruntime: positive MaxSteps and explicit ModelLimits are required")
	}
	if err := in.ModelLimits.Validate(true); err != nil {
		return nil, fmt.Errorf("chatruntime: %w", err)
	}
	if !in.DirectTools && in.ExecutorFactory == nil {
		return nil, errors.New("chatruntime: ExecutorFactory is required for JavaScript")
	}
	if in.Resume != nil && (in.Resume.Reason != "permission" || in.Approved == nil) {
		return nil, errors.New("chatruntime: resume requires a permission checkpoint and explicit approval")
	}
	if in.ForceCompact && in.Resume != nil {
		return nil, errors.New("chatruntime: compaction and resume are mutually exclusive")
	}
	if err := validateCapabilities(in.Capabilities); err != nil {
		return nil, err
	}
	prompt, err := RenderPrompt(in.Instructions, in.Capabilities, in.DirectTools)
	if err != nil {
		return nil, err
	}
	rt := &runtime{input: in, ctx: ctx}
	defer func() {
		if rt.executor != nil {
			err = errors.Join(err, rt.executor.Close())
		}
	}()
	b := bus.New()
	unsub := eventstream.Forward(b, in.Sink)
	defer unsub()
	runner := sol.NewRunner(sol.RunnerOptions{
		Agent: &agent.Agent{
			Name: "chat", MaxSteps: in.MaxSteps, Temperature: in.Temperature,
			SystemPrompt: prompt,
			Tools:        rt.tools(), Redactor: in.Redactor,
			HistoryPolicy: agent.HistoryPolicy{FilesRetainTurns: 0},
		},
		Model: in.Model, ModelLimits: in.ModelLimits, SessionStore: in.SessionStore,
		Bus: b, Quiet: true, ToolCallExecutionMode: stream.ToolCallExecutionSync,
		CompactionConfig: &session.CompactionConfig{Auto: true, Prune: true, PrunedMessage: func(info session.PrunedInfo) string {
			key := info.Source
			if key == "" {
				key = info.Filename
			}
			if key == "" {
				return session.DefaultPrunedMessage(info)
			}
			return fmt.Sprintf("[Attachment %q is detached to save context. Its contents are unavailable. Reload with air.attachToContext({path: %q}) using await in run_js, or air__attach_to_context in direct mode.]", key, key)
		}},
	})
	if in.ForceCompact {
		compacted, err := runner.Compact(ctx)
		if err != nil {
			return nil, err
		}
		text := fmt.Sprintf("Context compacted. %d tokens freed.", compacted.TokensFreed)
		in.Sink.OnTextDelta(stream.TextDeltaEvent{Text: text})
		return &sol.RunResult{Status: sol.RunCompleted, TotalText: text}, nil
	}
	var resumedMessages []goai.Message
	if in.Resume != nil {
		resolved, err := runner.ResolvePermissionSuspension(ctx, in.Resume, *in.Approved)
		if err != nil {
			return nil, err
		}
		if resolved.SuspensionContext != nil {
			history, err := in.SessionStore.Load(ctx)
			if err != nil {
				return nil, fmt.Errorf("load suspended history: %w", err)
			}
			in.Sink.OnSuspension(resolved.SuspensionContext)
			return &sol.RunResult{Status: sol.RunSuspended, SuspensionContext: resolved.SuspensionContext, Messages: session.MessagesToGoAI(history), NewMessages: resolved.Messages}, nil
		}
		resumedMessages = resolved.Messages
		if *in.Approved {
			in.Message = ""
		}
	}
	result, err = runner.Run(ctx, in.Message)
	if result != nil && len(resumedMessages) != 0 {
		result.NewMessages = append(resumedMessages, result.NewMessages...)
	}
	if result != nil && result.SuspensionContext != nil {
		in.Sink.OnSuspension(result.SuspensionContext)
	}
	return result, err
}
