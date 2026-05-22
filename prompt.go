package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	goai "github.com/airlockrun/goai"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
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
		var input PromptInput
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
		// external-MCP prompt calls. CheckFileAccess uses them to gate
		// reads on scoped directories (run-<parent>/, user-<id>/, etc.)
		// to the originating call context.
		parentRunID := r.Header.Get("X-Parent-Run-ID")
		userID := r.Header.Get("X-User-ID")

		run := newRun(agent, runID, bridgeID, input.ConversationID, ctx)
		run.parentRunID = parentRunID
		run.userID = userID
		// Stash the per-turn access level for vm.go's bind-time gating.
		// Empty defaults to AccessUser (safest broad default for a /prompt).
		if input.CallerAccess != "" {
			run.callerAccess = input.CallerAccess
		} else {
			run.callerAccess = AccessUser
		}
		run.autoConfirm = input.AutoConfirm
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
				run.complete(ctx, "error", errMsg, ErrorKindAgent, trace)
				return
			}
		}()

		// Build Sol agent with agentsdk tools.
		solAgent := newSolAgent(agent, run, input.SupportedModalities)

		// Airlock composes access-filtered extras at run dispatch; append to
		// the sync-cached base prompt so the LLM sees everything in one
		// system message.
		if input.ExtraSystemPrompt != "" {
			if solAgent.SystemPrompt != "" {
				solAgent.SystemPrompt += "\n\n"
			}
			solAgent.SystemPrompt += input.ExtraSystemPrompt
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
			opts.SessionStore = NewHTTPSessionStore(agent.client, input.ConversationID, runID, input.Source)
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
				run.complete(ctx, "error", err.Error(), ErrorKindPlatform, "")
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
			// Load checkpoint to get pending tool calls. Messages /
			// compactionState are captured raw so a re-suspension (a
			// multi-step delegated confirmation) can re-persist the
			// parent's history verbatim with only the suspension
			// context swapped.
			var checkpoint struct {
				Messages          json.RawMessage        `json:"messages"`
				SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
				CompactionState   json.RawMessage        `json:"compactionState"`
			}
			_ = agent.client.doJSON(ctx, "GET", "/api/agent/run/"+input.ResumeRunID+"/checkpoint", nil, &checkpoint)

			// Resolve the gate with the human's decision, then append
			// results to store. A "delegated" suspension drives the
			// child (A2A sibling / in-process subagent) with the
			// decision instead of locally allow/deny-ing a tool — the
			// down-cascade of tree suspension.
			if checkpoint.SuspensionContext != nil {
				approved := input.Approved != nil && *input.Approved
				if checkpoint.SuspensionContext.Reason == "delegated" {
					if reSusp := resolveDelegatedSuspension(ctx, agent, run.id, opts, checkpoint.SuspensionContext, approved, input.Message, opts.SessionStore, ew); reSusp != nil {
						// The child consumed this decision and hit its
						// next gate. Re-suspend the parent with the new
						// delegated context (same as gate 1) so the
						// conversation keeps a resumable suspension and
						// the next approval chains — instead of running
						// to success with the gate only narrated.
						//
						// This path never runs the Sol runner, so nothing
						// else records the human's resume message. Persist
						// it here (skipping the synthetic "Rejected by
						// user." nudge, which arrives as source="control")
						// — otherwise the resume run stores zero messages
						// and the user's reply is silently dropped from the
						// conversation thread.
						if prompt != "" && input.Source != "control" {
							ew.writeLine(ndjsonLine{
								Type: "messages",
								Data: []message.Message{message.NewUserMessage(prompt)},
							})
						}
						emitSuspensionEvent(ew, reSusp)
						ckpt, _ := json.Marshal(map[string]any{
							"messages":          checkpoint.Messages,
							"suspensionContext": reSusp,
							"compactionState":   checkpoint.CompactionState,
						})
						run.completeWithCheckpoint(ctx, "suspended", "", "", "", ckpt)
						return
					}
				} else if len(checkpoint.SuspensionContext.PendingToolCalls) > 0 {
					resolvePendingToolCalls(ctx, solAgent.Tools, opts.SessionStore, checkpoint.SuspensionContext.PendingToolCalls, approved, ew)
				}
			}

			// When not using the store, also load checkpoint messages.
			if opts.SessionStore == nil {
				var msgCheckpoint struct {
					Messages []goai.Message `json:"messages"`
				}
				if err := agent.client.doJSON(ctx, "GET", "/api/agent/run/"+input.ResumeRunID+"/checkpoint", nil, &msgCheckpoint); err == nil && len(msgCheckpoint.Messages) > 0 {
					opts.InitialMessages = msgCheckpoint.Messages
				}
			}

			runner := sol.NewRunner(opts)

			// Stream bus events to NDJSON.
			unsub := streamBusToNDJSON(runBus, ew)
			defer unsub()

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
	if err != nil {
		ew.WriteError(err)
		run.complete(ctx, "error", err.Error(), ErrorKindPlatform, "")
		return
	}

	// Emit rich messages for Airlock to store (before finish/suspended signals).
	if len(result.NewMessages) > 0 {
		ew.writeLine(ndjsonLine{
			Type: "messages",
			Data: result.NewMessages,
		})
	}

	switch result.Status {
	case sol.RunSuspended:
		emitSuspensionEvent(ew, result.SuspensionContext)
		// Serialize checkpoint: messages + suspension context + compaction state.
		checkpoint, _ := json.Marshal(map[string]any{
			"messages":          result.Messages,
			"suspensionContext": result.SuspensionContext,
			"compactionState":   result.CompactionState,
		})
		run.completeWithCheckpoint(ctx, "suspended", "", "", "", checkpoint)
	case sol.RunCancelled:
		// Cancellation is user-initiated, neither platform nor agent fault.
		run.complete(ctx, "error", "run cancelled", "", "")
	case sol.RunFailed:
		errMsg := ""
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		run.complete(ctx, "error", errMsg, ErrorKindPlatform, "")
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

// resolvePendingToolCalls executes (or denies) pending tool calls from a suspended
// run and appends the results to the session store so the LLM sees complete pairs.
func resolvePendingToolCalls(
	ctx context.Context,
	tools tool.Set,
	store session.SessionStore,
	pending []stream.ToolCall,
	approved bool,
	ew *EventWriter,
) {
	// Set up context with permissive permission manager so tools that call
	// pm.Ask() (e.g. run_js with request_confirmation) don't suspend again.
	permBus := bus.New()
	pm := bus.NewPermissionManager(permBus)
	pm.AddRule(bus.PermissionRule{Permission: "*", Pattern: "*", Action: "allow"})
	toolCtx := bus.WithBus(ctx, permBus)
	toolCtx = bus.WithPermissionManager(toolCtx, pm)

	var resultMsgs []session.Message

	for _, tc := range pending {
		var toolOut goai.ToolResultOutput

		if approved {
			t, ok := tools[tc.Name]
			if !ok {
				toolOut = goai.ErrorTextOutput{Value: "unknown tool " + tc.Name}
			} else {
				result, err := t.Execute(toolCtx, tc.Input, tool.CallOptions{ToolCallID: tc.ID})
				if err != nil {
					toolOut = tool.OutputForError(err)
				} else {
					toolOut = tool.SuccessOutput(result)
				}
			}
		} else {
			toolOut = goai.ExecutionDeniedOutput{Reason: "Execution was denied by the user."}
		}
		output := goai.ToolOutputWire(toolOut)

		// Emit tool result event so frontend/bridge sees it (kind reflects
		// the discriminated outcome).
		ew.WriteEvent(stream.ToolOutcomeEvent(tc.ID, tc.Name, nil, toolOut))

		resultMsgs = append(resultMsgs, session.Message{
			Role: "tool",
			Parts: []session.Part{{
				Type: "tool",
				Tool: &session.ToolPart{
					CallID:  tc.ID,
					Name:    tc.Name,
					Output:  output,
					Status:  "completed",
					Outcome: goai.ToolOutcome(toolOut),
				},
			}},
		})
	}

	// Append tool results to store — completes the orphaned assistant tool-call.
	if store != nil && len(resultMsgs) > 0 {
		_ = store.Append(ctx, resultMsgs)
	}
}

// resolveDelegatedSuspension is the down-cascade half of tree
// suspension: drive the delegated child (A2A sibling or in-process Sol
// subagent) to a terminal state with the human's decision, then emit +
// persist the suspended parent tool call's result so the resumed run
// continues. The up-cascade half is bus.ErrDelegatedSuspend →
// runner.handleSuspension.
// resolveDelegatedSuspension returns a non-nil *sol.SuspensionContext when
// the delegated child re-suspended (a multi-step confirmation: it consumed
// this decision and immediately hit its next gate). The caller must
// re-suspend the parent run with that context instead of continuing —
// otherwise the run ends success with no resumable suspension and the
// next approval re-delegates from scratch. nil means the child reached a
// terminal state and its result was appended normally.
func resolveDelegatedSuspension(ctx context.Context, agent *Agent, callerRunID string, baseOpts sol.RunnerOptions, sc *sol.SuspensionContext, approved bool, denyMsg string, store session.SessionStore, ew *EventWriter) *sol.SuspensionContext {
	rawData, _ := json.Marshal(sc.Data)
	var del struct {
		ToolCallID string          `json:"toolCallID"`
		Transport  string          `json:"transport"`
		Child      json.RawMessage `json:"child"`
	}
	_ = json.Unmarshal(rawData, &del)

	var output string
	var failed bool // structured (no text sniffing): a real delegation error
	switch del.Transport {
	case "a2a":
		var ch struct {
			AgentID string `json:"agentId"`
			Slug    string `json:"slug"`
			TaskID  string `json:"taskId"`
		}
		_ = json.Unmarshal(del.Child, &ch)
		aid, perr := uuid.Parse(ch.AgentID)
		if perr != nil {
			output = "Error: invalid delegated agent id: " + perr.Error()
			failed = true
			break
		}
		h := &SiblingHandle{slug: ch.Slug, agentID: aid, agent: agent}
		decision := "deny"
		if approved {
			decision = "approve"
		}
		args := map[string]any{"taskId": ch.TaskID, "decision": decision}
		if !approved && denyMsg != "" {
			args["message"] = denyMsg
		}
		res, cerr := h.CallTool(ctx, callerRunID, "prompt", args)
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
					return &sol.SuspensionContext{
						Reason:           "delegated",
						Data:             &bus.ErrDelegatedSuspend{ToolCallID: del.ToolCallID, Transport: "a2a", Child: nextChild},
						PendingToolCalls: sc.PendingToolCalls,
						CompletedResults: sc.CompletedResults,
					}
				}
			}
			if b, mErr := json.Marshal(res); mErr == nil {
				output = string(b)
			} else {
				output = fmt.Sprintf("%v", res)
			}
		}
	case "inprocess":
		text, reSusp := resumeInProcessChild(ctx, agent, callerRunID, baseOpts, del.Child, approved, denyMsg, ew)
		if reSusp != nil {
			// Associate with the parent's pending tool call (the
			// runner stamps ToolCallID on the up-cascade; on the
			// resume path we set it explicitly, like the a2a branch)
			// and re-suspend the parent — same shape as gate 1.
			reSusp.ToolCallID = del.ToolCallID
			return &sol.SuspensionContext{
				Reason:           "delegated",
				Data:             reSusp,
				PendingToolCalls: sc.PendingToolCalls,
				CompletedResults: sc.CompletedResults,
			}
		}
		output = text
	default:
		output = "Error: unknown delegated transport: " + del.Transport
		failed = true
	}

	toolName := "promptAgent"
	for _, tc := range sc.PendingToolCalls {
		if tc.ID == del.ToolCallID {
			toolName = tc.Name
			break
		}
	}
	var toolOut goai.ToolResultOutput = goai.TextOutput{Value: output}
	if failed {
		toolOut = goai.ErrorTextOutput{Value: output}
	}
	ew.WriteEvent(stream.ToolOutcomeEvent(del.ToolCallID, toolName, nil, toolOut))
	if store != nil {
		_ = store.Append(ctx, []session.Message{{
			Role: "tool",
			Parts: []session.Part{{
				Type: "tool",
				Tool: &session.ToolPart{
					CallID:  del.ToolCallID,
					Name:    toolName,
					Output:  output,
					Status:  "completed",
					Outcome: goai.ToolOutcome(toolOut),
				},
			}},
		}})
	}
	return nil
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
func resumeInProcessChild(ctx context.Context, agent *Agent, callerRunID string, baseOpts sol.RunnerOptions, childRaw json.RawMessage, approved bool, denyMsg string, ew *EventWriter) (string, *bus.ErrDelegatedSuspend) {
	var child struct {
		AgentName         string                 `json:"agentName"`
		Messages          []goai.Message         `json:"messages"`
		SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
		CompactionState   *sol.CompactionState   `json:"compactionState"`
	}
	if err := json.Unmarshal(childRaw, &child); err != nil {
		return "Error: decode in-process child: " + err.Error(), nil
	}
	factory, ok := solagent.GetFactory(child.AgentName)
	if !ok {
		return "Error: subagent type not found on resume: " + child.AgentName, nil
	}
	sub := factory("")
	subOpts := baseOpts // inherit model / apiKey / baseURL / bus
	subOpts.Agent = sub
	subOpts.InitialMessages = child.Messages
	subOpts.SessionStore = nil
	subRunner := sol.NewRunner(subOpts)

	if child.SuspensionContext != nil {
		if child.SuspensionContext.Reason == "delegated" {
			// The subagent itself delegated onward. If THAT child
			// re-suspended (deeper multi-step), the subagent stays
			// gated — re-package it (messages unchanged; new nested
			// context) so the parent re-suspends. Don't run the
			// subagent: it can't proceed until its delegate resolves.
			if rs := resolveDelegatedSuspension(ctx, agent, callerRunID, baseOpts, child.SuspensionContext, approved, denyMsg, nil, ew); rs != nil {
				return "", &bus.ErrDelegatedSuspend{
					Transport: "inprocess",
					Child: sol.InProcessChild{
						AgentName:         child.AgentName,
						Messages:          child.Messages,
						SuspensionContext: rs,
						CompactionState:   child.CompactionState,
					},
				}
			}
		} else if len(child.SuspensionContext.PendingToolCalls) > 0 {
			resolvePendingToolCalls(ctx, sub.Tools, nil, child.SuspensionContext.PendingToolCalls, approved, ew)
		}
	}
	resumePrompt := ""
	if !approved && denyMsg != "" {
		resumePrompt = denyMsg
	}
	res, err := subRunner.Run(ctx, resumePrompt)
	if err != nil {
		return "Error: subagent resume: " + err.Error(), nil
	}
	if res == nil {
		return "", nil
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
		}
	}
	return res.TotalText, nil
}
