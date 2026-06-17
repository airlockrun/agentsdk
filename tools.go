package agentsdk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/bus"
	"github.com/google/uuid"
)

type runJSInput struct {
	Code                string `json:"code" jsonschema:"description=JavaScript code to execute. Return the result of the last expression."`
	RequestConfirmation bool   `json:"request_confirmation,omitempty" jsonschema:"description=Set to true if this code modifies external data or has side effects the user should review first. The code will NOT execute — instead the user will be shown the code and asked to approve."`
}

// buildSolTools creates the tool.Set for Sol's Runner. In the default
// (JS) surface every capability is a goja binding inside one `run_js`
// tool — minimal LLM surface, maximal composability. In direct-tools
// mode (run.directTools, set by airlock for public-tier callers today)
// each capability is its own typed LLM tool and run_js is absent —
// narrower attack surface and predictable single-call exchanges. See
// buildDirectTools in directtools.go for the direct-tools shape.
func buildSolTools(agent *Agent, run *run, supportedModalities []string) tool.Set {
	var ts tool.Set
	if run.directTools {
		ts = buildDirectTools(agent, run, supportedModalities)
	} else {
		ts = tool.Set{
			"run_js": buildRunJSTool(agent, run),
		}
	}

	// promptAgent: open-ended A2A delegation as a first-class tool (not
	// a run_js binding). A suspendable sibling LLM-loop round-trip must
	// be its own pending tool call so Sol's suspend/resume handles it
	// natively (no JS-sandbox re-run / idempotency problem). Only
	// registered when this run can see at least one sibling. Surfaces
	// identically in both modes — the JS/direct split governs the
	// per-capability surface, not the open-ended delegation primitive.
	if t, ok := buildPromptAgentTool(agent, run); ok {
		ts["promptAgent"] = t
	}

	// Wrap all tool Execute functions with output truncation.
	for name, t := range ts {
		ts[name] = wrapWithTruncation(t, run)
	}

	return ts
}

// runJSDescription is the LLM-facing description of the run_js tool. Tool
// descriptions ride with every tool call envelope, so this stays short:
// the system prompt's "JavaScript environment" and "Built-in functions"
// sections carry the runtime rules, the var/let guidance, the
// request_confirmation policy, and every binding's signature — there's no
// need to repeat them per-tool.
const runJSDescription = `Execute JavaScript in the agent's sandboxed runtime.

The system prompt's "JavaScript environment" and "Built-in functions" sections describe the runtime semantics (ES5.1 + select ES6, no async/await, ` + "`var`" + ` for top-level names, last-expression-returned), every built-in binding, and the request_confirmation policy. Read those first.

Parameters:
- code: JS source to execute. The value of the last expression is the result; do NOT use a top-level ` + "`return`" + ` statement.
- request_confirmation: set true ONLY for code that modifies external data (sending messages, deleting records, spending money). Read-only operations and lookups must NEVER set this. When set, the user sees the commented code and decides whether to approve — comment every line so they can understand exactly what will happen.`

// buildRunJSTool creates the run_js tool for a given agent and run.
func buildRunJSTool(agent *Agent, run *run) tool.Tool {
	return tool.New("run_js").
		Description(runJSDescription).
		SchemaFromStruct(runJSInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var args runJSInput
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Result{}, fmt.Errorf("invalid run_js input: %w", err)
			}

			// If confirmation requested, ask the permission manager.
			// This triggers Sol's suspension mechanism (ErrPermissionNeeded)
			// if no rule allows it. The run suspends, the user is asked
			// to confirm, and on resume the permission rule is added.
			//
			// autoConfirm runs have no interactive second turn in which to
			// answer a confirmation (public one-shot bridge sessions), so the
			// gate is skipped and the code executes — the LLM's
			// request_confirmation intent is honored as "just run it".
			if args.RequestConfirmation && !run.autoConfirm {
				pm := bus.PermissionManagerFromContext(ctx)
				err := pm.Ask(ctx, bus.PermissionRequest{
					Permission: "run_js",
					// "*" is a match-anything placeholder — run_js has no
					// meaningful per-request pattern. Rules can still
					// auto-allow/deny all run_js via {permission=run_js
					// pattern=*}. The full code is carried in Metadata for
					// observability, and rendered in the confirmation UI
					// via the PermissionAskedPayload's Code field — so we
					// don't duplicate it into Patterns.
					Patterns:   []string{"*"},
					ToolCallID: opts.ToolCallID,
					Metadata:   map[string]any{"code": args.Code},
				})
				if err != nil {
					// ErrPermissionNeeded → Sol suspends the run
					// PermissionDeniedError → tool returns denial to LLM
					if _, ok := err.(*bus.PermissionDeniedError); ok {
						run.recordAction("run_js_denied", map[string]string{"code": args.Code}, "denied", nil, 0)
						return tool.Result{}, tool.DeniedError{Reason: "Code execution was denied by the user."}
					}
					return tool.Result{}, err
				}
			}

			// Clear pending logs before execution.
			run.mu.Lock()
			run.pendingLogs = nil
			run.mu.Unlock()

			// Guard the JS execution: relays run.ctx cancellation
			// (handlePrompt's WithTimeout / disconnect) AND enforces the L3
			// ceilings — process heap growth and JS-attributable CPU time
			// (wall minus time parked in Go calls, so a long legit download
			// is never charged as a spin). goja.Interrupt aborts the
			// in-flight run with an error that propagates out normally.
			vm := run.vmRuntime()
			stopGuard := startJSGuard(run.ctx, vm, run.gw)

			start := time.Now()
			result, err := executeJS(vm, args.Code)
			stopGuard()
			duration := time.Since(start)

			// Drain logs from this execution.
			run.mu.Lock()
			logs := run.pendingLogs
			run.pendingLogs = nil
			run.mu.Unlock()

			if err != nil {
				msg, native := jsErrorMessage(err)
				if native {
					msg = "(native) " + msg
				}
				err = &jsRunError{msg: msg, cause: err}
			}

			// Record action.
			run.recordAction("run_js", map[string]string{"code": args.Code}, result, err, duration)

			if err != nil {
				// Return the error so the executor classifies the outcome
				// as a tool error (red), not a successful text result that
				// merely starts with "Error:". The executor feeds it back
				// to the model (non-fatal) and prepends its own "Error: ".
				return tool.Result{}, err
			}

			// Combine console output + return value.
			output := combineJSOutput(logs, result)

			// Drain any attachments collected via attachToContext() so they
			// get injected as real image/file parts on the next LLM turn.
			run.mu.Lock()
			attachments := run.pendingAttachments
			run.pendingAttachments = nil
			run.mu.Unlock()

			return tool.Result{Output: output, Attachments: attachments}, nil
		}).
		Build()
}

type promptAgentInput struct {
	Agent     string     `json:"agent" jsonschema:"description=Slug of the sibling agent to delegate to (see the Sibling agents section of the system prompt)."`
	Message   string     `json:"message" jsonschema:"description=The task / message to send. When resuming an input-required task, put the answer here."`
	ContextID string     `json:"contextId,omitempty" jsonschema:"description=Opaque thread handle, minted by the target agent. OMIT on the first call to an agent. Only ever pass a contextId you got back from THIS agent's own prior promptAgent result — never your own run/conversation id, never a fabricated value."`
	TaskID    string     `json:"taskId,omitempty" jsonschema:"description=Opaque task handle. Only set when resuming a task that returned state=input-required: pass that result's taskId verbatim and put the answer in message. Never invent one."`
	Files     []FilePath `json:"files,omitempty" jsonschema:"description=Paths in YOUR storage to attach. Each is copied into the target agent's storage and surfaced to it as an attached file (with content-type/size/filename derived from S3 — you only supply the path)."`
}

// buildPromptAgentTool builds the single top-level `promptAgent` tool —
// open-ended natural-language delegation to a visible sibling. ok=false
// (don't register) when this run sees no siblings. The visible set is
// frozen at run start, same as the vm.go sibling bindings.
func buildPromptAgentTool(agent *Agent, run *run) (tool.Tool, bool) {
	if len(run.visibleSiblings) == 0 {
		return tool.Tool{}, false
	}
	visible := make(map[uuid.UUID]struct{}, len(run.visibleSiblings))
	for _, id := range run.visibleSiblings {
		visible[id] = struct{}{}
	}
	run.agent.syncMu.RLock()
	allSibs := run.agent.promptData.Siblings
	run.agent.syncMu.RUnlock()

	type sib struct {
		id   uuid.UUID
		slug string
	}
	bySlug := make(map[string]sib)
	var listing strings.Builder
	for _, s := range allSibs {
		if _, ok := visible[s.ID]; !ok {
			continue
		}
		bySlug[s.Slug] = sib{id: s.ID, slug: s.Slug}
		fmt.Fprintf(&listing, "\n- %s — %s", s.Slug, s.Description)
	}
	if len(bySlug) == 0 {
		return tool.Tool{}, false
	}

	desc := "Delegate a natural-language task to a sibling agent; it runs its own LLM loop and returns " +
		"{text, taskId, contextId, state, artifacts}. `state` is completed or input-required " +
		"(if input-required, call again with the same taskId and the answer in message). " +
		"artifacts are files the sibling produced, copied into your storage. Available agents:" + listing.String()

	built := tool.New("promptAgent").
		Description(desc).
		SchemaFromStruct(promptAgentInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in promptAgentInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, fmt.Errorf("invalid promptAgent input: %w", err)
			}
			if in.Agent == "" || in.Message == "" {
				return tool.Result{}, errors.New("`agent` and `message` are required")
			}
			target, ok := bySlug[in.Agent]
			if !ok {
				return tool.Result{}, fmt.Errorf("unknown or unavailable agent: %s", in.Agent)
			}

			handle := &SiblingHandle{slug: target.slug, agentID: target.id, agent: agent}
			childArgs := map[string]any{"message": in.Message}
			if in.ContextID != "" {
				childArgs["contextId"] = in.ContextID
			}
			if in.TaskID != "" {
				childArgs["taskId"] = in.TaskID
			}
			if len(in.Files) > 0 {
				// Wire shape for A2A: [{path: "..."}]. Other FileInfo fields
				// (filename, contentType, size) get filled callee-side by
				// HeadObject during the cross-bucket copy — no point making
				// the caller LLM transcribe them.
				files := make([]map[string]string, len(in.Files))
				for i, p := range in.Files {
					files[i] = map[string]string{"path": string(p)}
				}
				childArgs["files"] = files
			}

			res, err := handle.CallTool(ctx, run.id, "prompt", childArgs)
			if err != nil {
				// failed / canceled surface as a normal tool error the
				// LLM can react to (matches the A2A error-channel design).
				// Returning the error (rather than burying it in a
				// success Output) is what the executor classifies as a
				// tool-error outcome — it still feeds the message back to
				// the model non-fatally.
				return tool.Result{}, err
			}

			// res is the structured {text,taskId,contextId,state,artifacts}.
			// On input-required, suspend this pending tool call as a
			// delegated suspension so it bubbles up the run tree and the
			// decision cascades back in on resume.
			if m, ok := res.(map[string]any); ok {
				if st, _ := m["state"].(string); st == "input-required" {
					child := map[string]any{
						"agentId": target.id.String(),
						"slug":    target.slug,
						"taskId":  m["taskId"],
					}
					// Leaf gate detail so the root confirmation the
					// human sees says WHAT this sibling wants to do.
					if c, ok := m["confirmation"]; ok && c != nil {
						child["confirmation"] = c
					}
					return tool.Result{}, &bus.ErrDelegatedSuspend{
						ToolCallID: opts.ToolCallID,
						Transport:  "a2a",
						Child:      child,
					}
				}
			}

			out, mErr := json.Marshal(res)
			if mErr != nil {
				return tool.Result{Output: fmt.Sprintf("%v", res)}, nil
			}
			return tool.Result{Output: string(out)}, nil
		}).
		Build()
	return built, true
}

// mimeMatchesModalities checks if a MIME type is supported by the given modalities.
// Modalities are high-level: "image", "pdf", "audio", "video".
func mimeMatchesModalities(mimeType string, modalities []string) bool {
	for _, m := range modalities {
		switch m {
		case "image":
			if strings.HasPrefix(mimeType, "image/") {
				return true
			}
		case "pdf":
			if mimeType == "application/pdf" {
				return true
			}
		case "audio":
			if strings.HasPrefix(mimeType, "audio/") {
				return true
			}
		case "video":
			if strings.HasPrefix(mimeType, "video/") {
				return true
			}
		}
	}
	return false
}

const (
	// maxToolOutputLen is the maximum length of a tool output before truncation.
	// ~16KB keeps the LLM context manageable while showing enough data shape.
	maxToolOutputLen = 16 * 1024

	// truncatePreviewLen is how much of the original output to keep as a preview.
	truncatePreviewLen = 2 * 1024
)

// wrapWithTruncation wraps a tool's Execute function to truncate large outputs.
func wrapWithTruncation(t tool.Tool, run *run) tool.Tool {
	original := t.Execute
	t.Execute = func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		result, err := original(ctx, input, opts)
		if err != nil {
			return result, err
		}
		result.Output = truncateToolOutput(ctx, run, result.Output)
		return result, nil
	}
	return t
}

// truncateToolOutput saves large outputs to S3 and returns a truncated version
// with instructions for the LLM on how to access the full data.
func truncateToolOutput(ctx context.Context, run *run, output string) string {
	if len(output) <= maxToolOutputLen {
		return output
	}

	// Save full output to the framework-owned /tmp directory. The LLM reads
	// it back via fileRead(path) inside run_js.
	path := reservedTmpPath + "/output-" + randomHex(4) + ".txt"
	if _, err := run.agent.WriteFile(ctx, path, strings.NewReader(output), "text/plain"); err != nil {
		// If save fails, just truncate without a path.
		return output[:truncatePreviewLen] + fmt.Sprintf(
			"\n\n[Output truncated (%dKB). Could not save full result.]",
			len(output)/1024)
	}

	return output[:truncatePreviewLen] + fmt.Sprintf(
		"\n\n[Output truncated (%dKB → %dKB shown). Full result saved at %q.\n"+
			"Process it inside run_js without returning the full content:\n"+
			"  var data = fileRead(%q)\n"+
			"  var parsed = JSON.parse(data) // or process as text\n"+
			"  parsed.slice(0, 10)           // last expression = return value; only what you need\n"+
			"]",
		len(output)/1024, truncatePreviewLen/1024, path, path)
}

// combineJSOutput merges console.log output with the return value, similar
// to how a browser console shows logged output then the expression result.
// Levels above info are prefixed so the LLM can distinguish a console.warn
// from a plain log; info lines come through verbatim.
func combineJSOutput(logs []LogEntry, result string) string {
	if len(logs) == 0 {
		return result
	}
	parts := make([]string, len(logs))
	for i, l := range logs {
		switch l.Level {
		case LogLevelWarn:
			parts[i] = "[warn] " + l.Message
		case LogLevelError:
			parts[i] = "[error] " + l.Message
		default:
			parts[i] = l.Message
		}
	}
	combined := strings.Join(parts, "\n")
	if result != "" {
		combined += "\n" + result
	}
	return combined
}

func randomHex(n int) string {
	b := make([]byte, n)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}
