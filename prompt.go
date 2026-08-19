package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	goai "github.com/airlockrun/goai"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	sol "github.com/airlockrun/sol"
	solagent "github.com/airlockrun/sol/agent"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const maxToolSteps = 50

// promptTimeout is the agent-side hard ceiling on a single /prompt
// request. The practical deadline lives on Airlock's side (a per-run timer
// armed at 2 min by ForwardPrompt and pushed by ExtendRun, capped at
// MaxExtensions × ExtendIncrement); when that timer fires, Airlock cancels
// the request ctx and the agent's r.Context() drains. This ceiling is
// purely defense in depth — covers the case where Airlock loses track of
// the run (process restart) and the agent would otherwise spin forever.
// Set generously above Airlock's PromptHTTPCeiling (35 min) plus grace.
const promptTimeout = 40 * time.Minute

// handlePrompt returns the HTTP handler for POST /prompt.
// Uses Sol's Runner for the thinking loop, with agentsdk tools (run_js, request_upgrade).
func handlePrompt(agent *Agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), promptTimeout)
		defer cancel()

		// Parse request body.
		var input wire.PromptInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Extract run ID from header — panic if missing (fail loud).
		runID := r.Header.Get("X-Run-ID")
		if runID == "" {
			panic("agentsdk: X-Run-ID header is required")
		}
		bridgeID := r.Header.Get("X-Bridge-ID")
		// X-Parent-Run-ID / X-User-ID are set by airlock for A2A and
		// external-MCP prompt calls. ResolveFilePath uses them to gate
		// reads on scoped directories (run-<parent>/, user-<id>/, etc.)
		// to the originating call context.
		parentRunID := r.Header.Get("X-Parent-Run-ID")
		userID := r.Header.Get("X-User-ID")

		run := newRun(agent, runID, bridgeID, input.ConversationID, ctx)
		run.parentRunID = parentRunID
		run.userID = userID
		run.platform = input.Platform
		run.userDisplayName = input.UserDisplayName
		run.userEmail = input.UserEmail
		// Stash the per-turn access level for vm.go's bind-time gating.
		// Empty defaults to AccessUser (safest broad default for a /prompt).
		if input.CallerAccess != "" {
			run.callerAccess = Access(input.CallerAccess)
		} else {
			run.callerAccess = AccessUser
		}
		run.autoConfirm = input.AutoConfirm
		run.directTools = input.DirectTools
		run.visibleSiblings = input.VisibleSiblings
		ctx = contextWithRun(ctx, run)

		// Build prompt text from user message + file metadata.
		prompt := input.Message
		if prompt == "" && len(input.Messages) > 0 {
			// Legacy: extract from last user message in Messages array.
			if last := input.Messages[len(input.Messages)-1]; last.Role == "user" {
				prompt = last.Content.Text
			}
		}
		// Attached-files info is NOT inlined here. Airlock writes it as
		// its own conversation message (trigger.PostFilesManifest,
		// source="llm") at every files-bearing ingress — one canonical
		// producer, in LLM context, hidden from the UI.

		ew := newEventWriter(w)

		// Panic recovery — record error and complete the run. Panics in the
		// /prompt path are tagged "agent": the LLM/sol path returns errors
		// (caught at the runner.Run / runner.Compact sites below) rather
		// than panicking, so a panic here implies an SDK bug or, more
		// commonly, agent code panicking through the goja VM bridge.
		defer func() {
			if rec := recover(); rec != nil {
				trace := string(debug.Stack())
				errMsg := fmt.Sprintf("%v", rec)
				agentLogger().Error("prompt panic", zap.String("error", errMsg), zap.String("stack", trace))
				ew.WriteError(fmt.Errorf("%s", errMsg))
				run.complete(ctx, "error", errMsg, wire.ErrorKindAgent, trace)
				return
			}
		}()

		// Commit the response before waiting on sync, session loading, or the
		// model. Airlock can then return the run ID and expose cancellation even
		// when the first NDJSON event takes a long time to arrive.
		ew.flushHeaders()

		// Self-heal a stale config cache. Airlock stamps its current config
		// fingerprint on every dispatch; a mismatch means a model-slot or slug
		// change landed without firing /refresh. Resync before building the
		// agent so THIS run already renders with fresh capabilities/modalities.
		// Best-effort: a resync failure logs and proceeds on the cached config
		// — a backstop that can't reach airlock must not kill the run.
		if input.ExpectedSyncHash != "" && input.ExpectedSyncHash != agent.syncedStateHash() {
			agentLogger().Warn("sync state drift — resyncing before run",
				zap.String("expected", input.ExpectedSyncHash),
				zap.String("cached", agent.syncedStateHash()))
			if err := agent.syncWithAirlock(ctx); err != nil {
				agentLogger().Error("self-heal resync failed; proceeding with cached config", zap.Error(err))
			}
		}

		// Build Sol agent with agentsdk tools.
		solAgent := newSolAgent(agent, run)

		// Airlock composes access-filtered extras at run dispatch; append to
		// the sync-cached base prompt so the LLM sees everything in one
		// system message.
		if input.Instructions != "" {
			if solAgent.SystemPrompt != "" {
				solAgent.SystemPrompt += "\n\n"
			}
			solAgent.SystemPrompt += input.Instructions
		}

		// Create scoped bus for this run.
		runBus := bus.New()

		// Build runner options.
		opts := sol.RunnerOptions{
			Agent: solAgent,
			Bus:   runBus,
			Quiet: true,
			Model: proxy.Model("", proxy.Options{
				BaseURL: run.agent.client.baseURL,
				Token:   run.agent.client.token,
				Headers: runIDHeader(runID),
			}),
			CompactionConfig: &session.CompactionConfig{
				Auto:  true,
				Prune: true,
				PrunedMessage: func(info session.PrunedInfo) string {
					key := info.Source
					if key == "" {
						key = info.Filename
					}
					if key == "" {
						return session.DefaultPrunedMessage(info)
					}
					switch info.Type {
					case "image":
						return fmt.Sprintf("[Image %s was attached earlier but has been detached to save context. You CAN NO LONGER see or analyze it. If the user asks about this image OR you need any data from it, call attachToContext(%q) inside run_js to reload it.]", key, key)
					case "file":
						return fmt.Sprintf("[File %s was attached earlier but has been detached to save context. You CAN NO LONGER read its contents. If the user asks about this file OR you need any data from it, call attachToContext(%q) inside run_js to reload it.]", key, key)
					default:
						return session.DefaultPrunedMessage(info)
					}
				},
			},
		}

		// Use SessionStore when conversation ID is available; fall back to InitialMessages.
		if input.ConversationID != "" {
			opts.SessionStore = newHTTPSessionStore(agent.client, input.ConversationID, runID, input.Source)
		} else {
			opts.InitialMessages = input.Messages
		}

		// Apply optional model parameters to the agent.
		if input.Temperature != nil {
			solAgent.Temperature = input.Temperature
		}

		// User-triggered compaction (/compact). Skip the thinking loop and run
		// Sol's summarization directly against the loaded history. Emits a
		// single text-delta + finish so Airlock's WS plumbing treats it like
		// a normal short run.
		if input.ForceCompact {
			runner := sol.NewRunner(opts)

			unsub := streamBusToNDJSON(runBus, ew)
			defer unsub()

			cr, err := runner.Compact(ctx)
			if err != nil {
				ew.WriteError(err)
				run.complete(ctx, "error", err.Error(), wire.ErrorKindPlatform, "")
				return
			}
			ew.writeLine(ndjsonLine{
				Type: "text-delta",
				Data: map[string]any{
					"text": fmt.Sprintf("Context compacted. %d tokens freed.", cr.TokensFreed),
				},
			})
			ew.writeLine(ndjsonLine{
				Type: "finish",
				Data: map[string]any{"finishReason": "stop"},
			})
			run.complete(ctx, "success", "", "", "")
			return
		}

		// If resuming a suspended run, execute pending tool calls then continue.
		if input.ResumeRunID != "" {
			var checkpoint struct {
				Messages          []goai.Message         `json:"messages"`
				SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
				CompactionState   *sol.CompactionState   `json:"compactionState"`
			}
			if err := agent.client.doJSON(ctx, "GET", "/api/agent/run/"+input.ResumeRunID+"/checkpoint", nil, &checkpoint); err != nil {
				failPromptRun(ctx, run, ew, fmt.Errorf("load resume checkpoint: %w", err))
				return
			}
			if err := validateResumeSuspension(checkpoint.SuspensionContext, "resume checkpoint"); err != nil {
				failPromptRun(ctx, run, ew, err)
				return
			}
			if opts.SessionStore == nil {
				opts.InitialMessages = checkpoint.Messages
			}

			runner := sol.NewRunner(opts)
			unsub := streamBusToNDJSON(runBus, ew)
			defer unsub()

			// Resolve the gate with the human's decision, then append
			// results to store. A "delegated" suspension drives the
			// child (A2A sibling / in-process subagent) with the
			// decision instead of locally allow/deny-ing a tool — the
			// down-cascade of tree suspension.
			approved := input.Approved != nil && *input.Approved
			switch checkpoint.SuspensionContext.Reason {
			case "delegated":
				resolution, err := resolveDelegatedSuspension(ctx, agent, run.id, opts, checkpoint.SuspensionContext, approved, input.Message, opts.SessionStore, ew)
				if err != nil {
					failPromptRun(ctx, run, ew, fmt.Errorf("resolve delegated suspension: %w", err))
					return
				}
				if opts.SessionStore == nil && len(resolution.Messages) > 0 {
					opts.InitialMessages = append(opts.InitialMessages, resolution.Messages...)
					runner = sol.NewRunner(opts)
				}
				if reSusp := resolution.SuspensionContext; reSusp != nil {
					messages := append([]goai.Message(nil), checkpoint.Messages...)
					if prompt != "" && input.Source != "control" {
						resumeMessage := goai.NewUserMessage(prompt)
						if opts.SessionStore != nil {
							if err := opts.SessionStore.Append(ctx, []session.Message{session.FromGoAIMessage(resumeMessage)}); err != nil {
								failPromptRun(ctx, run, ew, fmt.Errorf("store append delegated resume message: %w", err))
								return
							}
						}
						messages = append(messages, resumeMessage)
						ew.writeLine(ndjsonLine{
							Type: "messages",
							Data: []message.Message{message.NewUserMessage(prompt)},
						})
					}
					ckpt, err := marshalRunCheckpoint(messages, reSusp, checkpoint.CompactionState)
					if err != nil {
						failPromptRun(ctx, run, ew, err)
						return
					}
					emitSuspensionEvent(ew, reSusp)
					if err := run.completeWithCheckpoint(ctx, "suspended", "", "", "", ckpt); err != nil {
						ew.WriteError(fmt.Errorf("complete suspended run: %w", err))
					}
					return
				}
			case "permission":
				resolution, err := runner.ResolvePermissionSuspension(ctx, checkpoint.SuspensionContext, approved)
				if err != nil {
					failPromptRun(ctx, run, ew, fmt.Errorf("resolve permission suspension: %w", err))
					return
				}
				if resolution.SuspensionContext != nil {
					messages := append(append([]goai.Message(nil), checkpoint.Messages...), resolution.Messages...)
					ckpt, err := marshalRunCheckpoint(messages, resolution.SuspensionContext, checkpoint.CompactionState)
					if err != nil {
						failPromptRun(ctx, run, ew, err)
						return
					}
					emitSuspensionEvent(ew, resolution.SuspensionContext)
					if err := run.completeWithCheckpoint(ctx, "suspended", "", "", "", ckpt); err != nil {
						ew.WriteError(fmt.Errorf("complete suspended run: %w", err))
					}
					return
				}
			}

			// Resume — empty prompt continues from tool results,
			// user message if rejected so LLM re-reasons.
			resumePrompt := ""
			if input.Approved == nil || !*input.Approved {
				resumePrompt = prompt
			}

			result, err := runner.Run(ctx, resumePrompt)
			handleRunResult(ctx, run, ew, result, err)
			return
		}

		// Normal run.
		runner := sol.NewRunner(opts)

		// Stream bus events to NDJSON.
		unsub := streamBusToNDJSON(runBus, ew)
		defer unsub()

		result, err := runner.Run(ctx, prompt)

		handleRunResult(ctx, run, ew, result, err)
	}
}

// handleRunResult processes the Sol RunResult and completes the agentsdk run.
// All run-level error paths here originate from sol's runner — LLM stream
// failures, model lookup, internal sol errors — so they're tagged platform.
// Agent code that throws inside run_js never propagates here; goja errors
// are caught at the tool boundary and returned as tool.Result.
func handleRunResult(ctx context.Context, run *run, ew *EventWriter, result *sol.RunResult, err error) {
	if result != nil && len(result.NewMessages) > 0 {
		ew.writeLine(ndjsonLine{
			Type: "messages",
			Data: result.NewMessages,
		})
	}
	if err != nil {
		if result != nil && result.Status == sol.RunCancelled {
			run.complete(ctx, "error", "run cancelled", "", "")
			return
		}
		ew.WriteError(err)
		run.complete(ctx, "error", err.Error(), wire.ErrorKindPlatform, "")
		return
	}
	if result == nil {
		failPromptRun(ctx, run, ew, errors.New("sol returned no run result"))
		return
	}

	switch result.Status {
	case sol.RunSuspended:
		checkpoint, marshalErr := marshalRunCheckpoint(result.Messages, result.SuspensionContext, result.CompactionState)
		if marshalErr != nil {
			failPromptRun(ctx, run, ew, marshalErr)
			return
		}
		emitSuspensionEvent(ew, result.SuspensionContext)
		if err := run.completeWithCheckpoint(ctx, "suspended", "", "", "", checkpoint); err != nil {
			ew.WriteError(fmt.Errorf("complete suspended run: %w", err))
		}
	case sol.RunCancelled:
		// Cancellation is user-initiated, neither platform nor agent fault.
		run.complete(ctx, "error", "run cancelled", "", "")
	case sol.RunFailed:
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		run.complete(ctx, "error", errMsg, wire.ErrorKindPlatform, "")
	default:
		// Emit finish event so Airlock publishes run.complete to WS subscribers.
		// Shape matches ai-sdk v3 usage (inputTokens.total / outputTokens.total)
		// so the Airlock event publisher can parse it uniformly.
		finishPayload := map[string]any{
			"finishReason": "stop",
			"usage": map[string]any{
				"inputTokens":  map[string]any{"total": result.Usage.InputTotal()},
				"outputTokens": map[string]any{"total": result.Usage.OutputTotal()},
			},
		}
		ew.writeLine(ndjsonLine{
			Type: "finish",
			Data: finishPayload,
		})
		run.complete(ctx, "success", "", "", "")
	}
}

func marshalRunCheckpoint(messages []goai.Message, suspension *sol.SuspensionContext, compaction *sol.CompactionState) (json.RawMessage, error) {
	checkpoint, err := json.Marshal(struct {
		Messages          []goai.Message         `json:"messages"`
		SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
		CompactionState   *sol.CompactionState   `json:"compactionState"`
	}{
		Messages:          messages,
		SuspensionContext: suspension,
		CompactionState:   compaction,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal run checkpoint: %w", err)
	}
	return checkpoint, nil
}

func failPromptRun(ctx context.Context, run *run, ew *EventWriter, err error) {
	ew.WriteError(err)
	if completeErr := run.complete(ctx, "error", err.Error(), wire.ErrorKindPlatform, ""); completeErr != nil {
		ew.WriteError(fmt.Errorf("complete failed run: %w", completeErr))
	}
}

func validateResumeSuspension(suspension *sol.SuspensionContext, scope string) error {
	if suspension == nil {
		return fmt.Errorf("%s suspension context is required", scope)
	}
	switch suspension.Reason {
	case "permission", "delegated":
		return nil
	default:
		return fmt.Errorf("%s suspension reason %q is not supported", scope, suspension.Reason)
	}
}

// resolveDelegatedSuspension is the down-cascade half of tree
// suspension: drive the delegated child (A2A sibling or in-process Sol
// subagent) to a terminal state with the human's decision, then emit +
// persist the suspended parent tool call's result so the resumed run
// continues. The up-cascade half is bus.ErrDelegatedSuspend →
// runner.handleSuspension.
type delegatedResolution struct {
	Messages          []goai.Message
	SuspensionContext *sol.SuspensionContext
}

// resolveDelegatedSuspension returns a suspension when the child reaches its
// next gate. A terminal child produces one parent tool result message.
func resolveDelegatedSuspension(ctx context.Context, agent *Agent, callerRunID string, baseOpts sol.RunnerOptions, sc *sol.SuspensionContext, approved bool, denyMsg string, store session.SessionStore, ew *EventWriter) (*delegatedResolution, error) {
	if sc == nil || sc.Reason != "delegated" {
		return nil, errors.New("invalid delegated suspension")
	}
	rawData, err := json.Marshal(sc.Data)
	if err != nil {
		return nil, fmt.Errorf("encode delegated suspension: %w", err)
	}
	var del struct {
		ToolCallID string          `json:"toolCallID"`
		Transport  string          `json:"transport"`
		Child      json.RawMessage `json:"child"`
	}
	if err := json.Unmarshal(rawData, &del); err != nil {
		return nil, fmt.Errorf("decode delegated suspension: %w", err)
	}
	if del.ToolCallID == "" {
		return nil, errors.New("delegated suspension has no tool call ID")
	}
	if sc.ToolCallID != "" && sc.ToolCallID != del.ToolCallID {
		return nil, fmt.Errorf("delegated suspension tool call ID %q does not match data tool call ID %q", sc.ToolCallID, del.ToolCallID)
	}
	if len(sc.PendingToolCalls) == 0 {
		return nil, errors.New("delegated suspension has no pending tool calls")
	}
	if firstID := sc.PendingToolCalls[0].ID; firstID != del.ToolCallID {
		return nil, fmt.Errorf("delegated suspension current call %q is not first pending call %q", del.ToolCallID, firstID)
	}
	currentCall := sc.PendingToolCalls[0]
	foundCurrent := false
	for _, pending := range sc.PendingToolCalls {
		if pending.ID == del.ToolCallID {
			currentCall = pending
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		return nil, fmt.Errorf("delegated suspension current call %q is not pending", del.ToolCallID)
	}
	if store != nil {
		if _, err := store.Load(ctx); err != nil {
			return nil, fmt.Errorf("store load before delegated resolution: %w", err)
		}
	}

	var output string
	var failed bool // structured (no text sniffing): a real delegation error
	switch del.Transport {
	case "a2a":
		var ch struct {
			AgentID string `json:"agentId"`
			Slug    string `json:"slug"`
			TaskID  string `json:"taskId"`
		}
		if err := json.Unmarshal(del.Child, &ch); err != nil {
			return nil, fmt.Errorf("decode A2A delegated child: %w", err)
		}
		aid, perr := uuid.Parse(ch.AgentID)
		if perr != nil {
			return nil, fmt.Errorf("invalid delegated agent ID: %w", perr)
		}
		h := &siblingHandle{slug: ch.Slug, agentID: aid, agent: agent}
		decision := "deny"
		if approved {
			decision = "approve"
		}
		args := map[string]any{"taskId": ch.TaskID, "decision": decision}
		if !approved && denyMsg != "" {
			args["message"] = denyMsg
		}
		res, cerr := h.callTool(ctx, callerRunID, "prompt", args)
		switch {
		case cerr != nil:
			output = "Error: " + cerr.Error()
			failed = true
		default:
			// The child consumed this decision and immediately hit its
			// NEXT gate (multi-step confirmation). Re-raise a delegated
			// suspension so the parent re-suspends for the next approval
			// — mirrors buildPromptAgentTool's first-gate handling. Not
			// doing this flattens it to a completed result, the parent
			// ends success with no resumable suspension, and the next
			// approval re-delegates the sibling from scratch.
			if m, ok := res.(map[string]any); ok {
				if st, _ := m["state"].(string); st == "input-required" {
					nextChild := map[string]any{
						"agentId": ch.AgentID,
						"slug":    ch.Slug,
						"taskId":  m["taskId"],
					}
					if c, ok := m["confirmation"]; ok && c != nil {
						nextChild["confirmation"] = c
					}
					return &delegatedResolution{SuspensionContext: &sol.SuspensionContext{
						Reason:           "delegated",
						ToolCallID:       del.ToolCallID,
						Data:             &bus.ErrDelegatedSuspend{ToolCallID: del.ToolCallID, Transport: "a2a", Child: nextChild},
						PendingToolCalls: sc.PendingToolCalls,
						CompletedResults: sc.CompletedResults,
					}}, nil
				}
			}
			b, err := json.Marshal(res)
			if err != nil {
				return nil, fmt.Errorf("encode A2A delegated result: %w", err)
			}
			output = string(b)
		}
	case "inprocess":
		text, reSusp, err := resumeInProcessChild(ctx, agent, callerRunID, baseOpts, del.Child, approved, denyMsg, ew)
		if err != nil {
			return nil, err
		}
		if reSusp != nil {
			// Associate with the parent's pending tool call (the
			// runner stamps ToolCallID on the up-cascade; on the
			// resume path we set it explicitly, like the a2a branch)
			// and re-suspend the parent — same shape as gate 1.
			reSusp.ToolCallID = del.ToolCallID
			return &delegatedResolution{SuspensionContext: &sol.SuspensionContext{
				Reason:           "delegated",
				ToolCallID:       del.ToolCallID,
				Data:             reSusp,
				PendingToolCalls: sc.PendingToolCalls,
				CompletedResults: sc.CompletedResults,
			}}, nil
		}
		output = text
	default:
		return nil, fmt.Errorf("unknown delegated transport %q", del.Transport)
	}

	toolName := currentCall.Name
	var toolOut goai.ToolResultOutput = goai.TextOutput{Value: output}
	if failed {
		toolOut = goai.ErrorTextOutput{Value: output}
	}
	msg := goai.NewToolMessage(del.ToolCallID, toolName, toolOut)
	if store != nil {
		if err := store.Append(ctx, []session.Message{session.FromGoAIMessage(msg)}); err != nil {
			return nil, fmt.Errorf("store append delegated result %q: %w", del.ToolCallID, err)
		}
	}
	if err := ew.WriteEvent(stream.ToolOutcomeEvent(del.ToolCallID, toolName, currentCall.Input, toolOut, "", nil)); err != nil {
		return nil, fmt.Errorf("emit delegated result %q: %w", del.ToolCallID, err)
	}
	return &delegatedResolution{Messages: []goai.Message{msg}}, nil
}

// resumeInProcessChild reconstructs a suspended Sol subagent from the
// nested InProcessChild checkpoint, resolves its own gate with the same
// decision (recursing if it too delegated), runs it to terminal, and
// returns its final text. Model/provider config is inherited from the
// parent's resumed runner options, mirroring Runner.SpawnSubagent.
// resumeInProcessChild returns the subagent's terminal text, OR a non-nil
// *bus.ErrDelegatedSuspend when the subagent re-suspended (a multi-step
// gate). The envelope is byte-shape-identical to Runner.SpawnSubagent's
// up-cascade so the parent re-suspends and emitSuspensionEvent / resume
// handle it exactly like the first gate. ToolCallID is left unset here
// (no runner step on the resume path) — the caller stamps it.
func resumeInProcessChild(ctx context.Context, agent *Agent, callerRunID string, baseOpts sol.RunnerOptions, childRaw json.RawMessage, approved bool, denyMsg string, ew *EventWriter) (string, *bus.ErrDelegatedSuspend, error) {
	var child struct {
		AgentName         string                 `json:"agentName"`
		Messages          []goai.Message         `json:"messages"`
		SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
		CompactionState   *sol.CompactionState   `json:"compactionState"`
	}
	if err := json.Unmarshal(childRaw, &child); err != nil {
		return "", nil, fmt.Errorf("decode in-process child: %w", err)
	}
	if err := validateResumeSuspension(child.SuspensionContext, "in-process child checkpoint"); err != nil {
		return "", nil, err
	}
	factory, ok := solagent.GetFactory(child.AgentName)
	if !ok {
		return "", nil, fmt.Errorf("subagent type %q not found on resume", child.AgentName)
	}
	sub := factory("")
	subOpts := baseOpts
	subOpts.Agent = sub
	// Child gates remain private until the parent publishes the delegated
	// suspension carrying the parent tool call ID.
	subOpts.Bus = bus.New()
	subOpts.InitialMessages = child.Messages
	subOpts.SessionStore = nil
	subRunner := sol.NewRunner(subOpts)

	switch child.SuspensionContext.Reason {
	case "delegated":
		// The subagent itself delegated onward. If THAT child
		// re-suspended (deeper multi-step), the subagent stays
		// gated — re-package it (messages unchanged; new nested
		// context) so the parent re-suspends. Don't run the
		// subagent: it can't proceed until its delegate resolves.
		resolution, err := resolveDelegatedSuspension(ctx, agent, callerRunID, baseOpts, child.SuspensionContext, approved, denyMsg, nil, ew)
		if err != nil {
			return "", nil, fmt.Errorf("resolve nested delegated suspension: %w", err)
		}
		if resolution.SuspensionContext != nil {
			return "", &bus.ErrDelegatedSuspend{
				Transport: "inprocess",
				Child: sol.InProcessChild{
					AgentName:         child.AgentName,
					Messages:          append(append([]goai.Message(nil), child.Messages...), resolution.Messages...),
					SuspensionContext: resolution.SuspensionContext,
					CompactionState:   child.CompactionState,
				},
			}, nil
		}
		if len(resolution.Messages) > 0 {
			subOpts.InitialMessages = append(subOpts.InitialMessages, resolution.Messages...)
			subRunner = sol.NewRunner(subOpts)
		}
	case "permission":
		resolution, err := subRunner.ResolvePermissionSuspension(ctx, child.SuspensionContext, approved)
		if err != nil {
			return "", nil, fmt.Errorf("resolve in-process child permission: %w", err)
		}
		if resolution.SuspensionContext != nil {
			return "", &bus.ErrDelegatedSuspend{
				Transport: "inprocess",
				Child: sol.InProcessChild{
					AgentName:         child.AgentName,
					Messages:          append(append([]goai.Message(nil), child.Messages...), resolution.Messages...),
					SuspensionContext: resolution.SuspensionContext,
					CompactionState:   child.CompactionState,
				},
			}, nil
		}
	}
	resumePrompt := ""
	if !approved && denyMsg != "" {
		resumePrompt = denyMsg
	}
	res, err := subRunner.Run(ctx, resumePrompt)
	if err != nil {
		return "", nil, fmt.Errorf("subagent resume: %w", err)
	}
	if res == nil {
		return "", nil, nil
	}
	if res.Status == sol.RunSuspended {
		// The subagent hit its NEXT gate. Mirror SpawnSubagent's
		// up-cascade so the parent re-suspends and the decision
		// cascades back in on the next approval.
		return "", &bus.ErrDelegatedSuspend{
			Transport: "inprocess",
			Child: sol.InProcessChild{
				AgentName:         child.AgentName,
				Messages:          res.Messages,
				SuspensionContext: res.SuspensionContext,
				CompactionState:   res.CompactionState,
			},
		}, nil
	}
	return res.TotalText, nil, nil
}
