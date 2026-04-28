package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	goai "github.com/airlockrun/goai"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
	sol "github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
)

const maxToolSteps = 50

// promptTimeout caps a single /prompt request server-side. Set slightly
// below Airlock's 5-minute HTTP client timeout (airlock/trigger/dispatcher.go
// promptTimeout) so the agent has a 30-second head start to interrupt the
// VM, write a terminal run status, and finish the NDJSON stream cleanly
// before Airlock's client gives up.
const promptTimeout = 4*time.Minute + 30*time.Second

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

		run := newRun(agent, runID, bridgeID, input.ConversationID, ctx)
		// Stash the per-turn access level for vm.go's bind-time gating.
		// Empty defaults to AccessUser (safest broad default for a /prompt).
		if input.CallerAccess != "" {
			run.callerAccess = input.CallerAccess
		} else {
			run.callerAccess = AccessUser
		}
		ctx = contextWithRun(ctx, run)

		// Build prompt text from user message + file metadata.
		prompt := input.Message
		if prompt == "" && len(input.Messages) > 0 {
			// Legacy: extract from last user message in Messages array.
			if last := input.Messages[len(input.Messages)-1]; last.Role == "user" {
				prompt = last.Content.Text
			}
		}
		if len(input.Files) > 0 {
			var fileInfo string
			for _, f := range input.Files {
				zone, key, ok := strings.Cut(f.ID, "/")
				if !ok {
					// Defensive: airlock currently always emits "{zone}/{key}".
					// If a future caller passes a bare key, surface it as-is.
					zone, key = reservedTmpSlug, f.ID
				}
				fileInfo += fmt.Sprintf("- %s (%s, %d bytes) — zone: %q, key: %q\n", f.Filename, f.ContentType, f.Size, zone, key)
			}
			note := fmt.Sprintf("Attached files:\n%sUse storage_{zone}.get(key) in run_js to read text contents, or attachToContext({zone, key}) to load images/files into your visual context for the next turn.", fileInfo)
			prompt += "\n\n[" + note + "]"
		}

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
				log.Printf("agentsdk: prompt panic: %s\n%s", errMsg, trace)
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
			run.teardownStore()
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
			// Load checkpoint to get pending tool calls.
			var checkpoint struct {
				SuspensionContext *sol.SuspensionContext `json:"suspensionContext"`
			}
			_ = agent.client.doJSON(ctx, "GET", "/api/agent/run/"+input.ResumeRunID+"/checkpoint", nil, &checkpoint)

			// Execute or deny pending tool calls, then append results to store.
			if checkpoint.SuspensionContext != nil && len(checkpoint.SuspensionContext.PendingToolCalls) > 0 {
				approved := input.Approved != nil && *input.Approved
				resolvePendingToolCalls(ctx, solAgent.Tools, opts.SessionStore, checkpoint.SuspensionContext.PendingToolCalls, approved, ew)
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
			run.teardownStore()
			handleRunResult(ctx, run, ew, result, err)
			return
		}

		// Normal run.
		runner := sol.NewRunner(opts)

		// Stream bus events to NDJSON.
		unsub := streamBusToNDJSON(runBus, ew)
		defer unsub()

		result, err := runner.Run(ctx, prompt)
		run.teardownStore()

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
		var output string

		if approved {
			t, ok := tools[tc.Name]
			if !ok {
				output = "Error: unknown tool " + tc.Name
			} else {
				result, err := t.Execute(toolCtx, tc.Input, tool.CallOptions{ToolCallID: tc.ID})
				if err != nil {
					output = "Error: " + err.Error()
				} else {
					output = result.Output
				}
			}
		} else {
			output = "Execution was denied by the user."
		}

		// Emit tool result event so frontend/bridge sees it.
		ew.WriteEvent(stream.Event{
			Type: stream.EventToolResult,
			Data: stream.ToolResultEvent{
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Output:     stream.ToolOutput{Output: output},
			},
		})

		resultMsgs = append(resultMsgs, session.Message{
			Role: "tool",
			Parts: []session.Part{{
				Type: "tool",
				Tool: &session.ToolPart{
					CallID: tc.ID,
					Name:   tc.Name,
					Output: output,
					Status: "completed",
				},
			}},
		})
	}

	// Append tool results to store — completes the orphaned assistant tool-call.
	if store != nil && len(resultMsgs) > 0 {
		_ = store.Append(ctx, resultMsgs)
	}
}
