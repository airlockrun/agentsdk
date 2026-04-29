package agentsdk

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/bus"
)

type runJSInput struct {
	Code                string `json:"code" jsonschema:"description=JavaScript code to execute. Return the result of the last expression."`
	RequestConfirmation bool   `json:"request_confirmation,omitempty" jsonschema:"description=Set to true if this code modifies external data or has side effects the user should review first. The code will NOT execute — instead the user will be shown the code and asked to approve."`
}

// buildSolTools creates the tool.Set for Sol's Runner. All agent capabilities
// are exposed as JS functions inside the run_js VM — see vm.go. This keeps the
// LLM's tool surface minimal (one escape hatch) while still giving agents full
// composability (loops, data-flow chains, conditionals) in a single tool call.
func buildSolTools(agent *Agent, run *run, supportedModalities []string) tool.Set {
	ts := tool.Set{
		"run_js": buildRunJSTool(agent, run),
	}

	// Wrap all tool Execute functions with output truncation.
	for name, t := range ts {
		ts[name] = wrapWithTruncation(t, run)
	}

	return ts
}

// buildRunJSTool creates the run_js tool for a given agent and run.
func buildRunJSTool(agent *Agent, run *run) tool.Tool {
	return tool.New("run_js").
		Description(buildToolDescription(agent, run.callerAccess)).
		SchemaFromStruct(runJSInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var args runJSInput
			if err := json.Unmarshal(input, &args); err != nil {
				return tool.Result{Output: "Error: invalid input: " + err.Error()}, nil
			}

			// If confirmation requested, ask the permission manager.
			// This triggers Sol's suspension mechanism (ErrPermissionNeeded)
			// if no rule allows it. The run suspends, the user is asked
			// to confirm, and on resume the permission rule is added.
			if args.RequestConfirmation {
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
						return tool.Result{Output: "Code execution was denied by the user."}, nil
					}
					return tool.Result{}, err
				}
			}

			// Clear pending logs before execution.
			run.mu.Lock()
			run.pendingLogs = nil
			run.mu.Unlock()

			// Cancel the VM if the run's ctx fires (handlePrompt's WithTimeout,
			// or Airlock disconnecting). Without this, an infinite-loop or
			// runaway algorithm in LLM-generated JS spins at 100% CPU forever
			// — the goroutine outlives the request and bleeds into subsequent
			// prompts. goja.Interrupt aborts the in-flight RunString with an
			// *InterruptedError that propagates out as a regular error.
			vm := run.vmRuntime()
			done := make(chan struct{})
			go func() {
				select {
				case <-run.ctx.Done():
					vm.Interrupt(run.ctx.Err())
				case <-done:
				}
			}()

			start := time.Now()
			result, err := executeJS(vm, args.Code)
			close(done)
			duration := time.Since(start)

			// Drain logs from this execution.
			run.mu.Lock()
			logs := run.pendingLogs
			run.pendingLogs = nil
			run.mu.Unlock()

			// Record action.
			run.recordAction("run_js", map[string]string{"code": args.Code}, result, err, duration)

			if err != nil {
				return tool.Result{Output: "Error: " + err.Error()}, nil
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

	// Save full output to the framework-owned tmp zone. The LLM reads it
	// back via storage_tmp.get(...) inside run_js.
	relKey := "output-" + randomHex(4) + ".txt"
	tmp := &StorageHandle{slug: reservedTmpSlug, read: AccessUser, write: AccessUser, agent: run.agent}
	if err := tmp.Put(ctx, relKey, strings.NewReader(output), "text/plain"); err != nil {
		// If save fails, just truncate without a key.
		return output[:truncatePreviewLen] + fmt.Sprintf(
			"\n\n[Output truncated (%dKB). Could not save full result.]",
			len(output)/1024)
	}

	return output[:truncatePreviewLen] + fmt.Sprintf(
		"\n\n[Output truncated (%dKB → %dKB shown). Full result saved to storage_tmp at %q.\n"+
			"Process it inside run_js without returning the full content:\n"+
			"  let data = storage_tmp.get(%q)\n"+
			"  let parsed = JSON.parse(data) // or process as text\n"+
			"  return parsed.slice(0, 10)    // return only what you need\n"+
			"]",
		len(output)/1024, truncatePreviewLen/1024, relKey, relKey)
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

// buildToolDescription generates the run_js tool description including the function manifest.
// callerAccess gates admin-only bindings (queryDB/execDB) so they're not advertised
// to AccessUser / AccessPublic runs that wouldn't be able to call them anyway.
func buildToolDescription(agent *Agent, callerAccess Access) string {
	var b strings.Builder
	b.WriteString("Execute JavaScript code. The runtime is ES5.1 with select ES6 features (let/const, arrow functions, classes, destructuring, template literals, Map/Set) — there is NO `async`/`await` and NO Promises; using `await` is a syntax error. All bindings below are synchronous: call them directly, e.g. `const r = httpRequest(url)` not `await httpRequest(url)`.\n\n")
	b.WriteString("The value of the last expression is returned — do NOT use a top-level `return` statement (it's a syntax error outside a function). Write the value you want as the final expression. Example: `const r = httpRequest(url); r` returns `r`. `return r;` does NOT work.\n\n")
	b.WriteString("Variables declared with var/let/const persist across run_js calls within the same turn (the VM is reset only on the next user message), so you can build up state incrementally. Prefer `var` for top-level names — re-declaring a `let` or `const` with the same name in a follow-up run_js call throws SyntaxError. Reuse `var` names freely; reserve `let`/`const` for names you won't redeclare.\n\n")
	b.WriteString("Prefer ONE run_js call over many when it's safe. If a sequence is read-only or strictly additive (list → filter → look up one item → format), do it in a single call: fewer turns, less chance of stale state between calls, cheaper for the user. Split into multiple calls only when an earlier result must be inspected before deciding what to do next, or when an action is destructive enough that you want a confirmation gate between steps. Example: `const items = listX(); const target = items.find(i => i.name === 'foo'); if (!target.enabled) { updateX(target.id, {enabled: true}); } target` — one call, not three.\n\n")
	b.WriteString("IMPORTANT: request_confirmation parameter usage:\n")
	b.WriteString("- Set request_confirmation=true ONLY for code that modifies external data (sending messages, deleting records, spending money).\n")
	b.WriteString("- Read-only operations, data lookups, and computations must NEVER use request_confirmation — just execute them.\n")
	b.WriteString("- When requesting confirmation, add comments to the code explaining what it does so the user can make an informed decision.\n")
	b.WriteString("\nTools registered via `agent.RegisterTool` are declared as typed JS globals in the system prompt (not repeated here). Call them by name with a single object argument matching the declared input type.\n")

	// Built-in bindings.
	b.WriteString("\nBuilt-in bindings:\n")
	b.WriteString("- conn_{slug}.request(method, path, body?) → string — raw HTTP via connection\n")
	b.WriteString("- conn_{slug}.requestJSON(method, path, body?) → object — JSON HTTP via connection\n")
	b.WriteString("- mcp_{slug}.<tool_name>(args?) — call MCP tool. The per-tool typed methods are declared in the run-env prompt; one method per tool the server advertised at sync time.\n")
	if accessSatisfies(callerAccess, AccessAdmin) {
		b.WriteString("- queryDB(sql, ...params) → [{...}, ...] — admin-only; not exposed to AccessUser / AccessPublic callers.\n")
		b.WriteString("- execDB(sql, ...params) → {rowsAffected: N} — admin-only; not exposed to AccessUser / AccessPublic callers.\n")
	}
	b.WriteString("- storage_{slug}.get(key) → string — read a file's contents as a string. `key` is a relative key string or a {zone, key} ref handed back from another call.\n")
	b.WriteString("- storage_{slug}.put(key, data, contentType) — write a file.\n")
	b.WriteString("- storage_{slug}.stat(key) → {key, size, contentType, lastModified}\n")
	b.WriteString("- storage_{slug}.list(prefix?) → [{key, size, contentType, lastModified}, ...]\n")
	b.WriteString("- storage_{slug}.copy(srcKey, dstKey) — server-side copy within this zone.\n")
	b.WriteString("- storage_{slug}.copyTo(srcKey, dstRef) — server-side copy to a different zone; dstRef is a {zone, key} object.\n")
	b.WriteString("- storage_{slug}.delete(key)\n")
	b.WriteString("- printToUser(parts) — send rich content to user; parts is a single object or array of {type, text, source, url, data, filename, mimeType, alt, duration}. `source` accepts a {zone, key} ref or a \"zone/key\" string.\n")
	b.WriteString("- log(message)\n")
	b.WriteString("- webSearch(query, count?) → {results: [{title, url, snippet}], synthesis?, provider}\n")
	b.WriteString("- httpRequest(url, opts?) → {status, headers, body, contentType, size, savedTo?} — HTML responses are converted to markdown by default; binary and large responses auto-saved to S3. `savedTo` is a {zone, key} ref — pass it to storage_{zone}.get(...) to read text, or attachToContext(...) to view it next turn. Opts: {method, headers, body, timeout, saveAs, raw}; `saveAs` accepts a {zone, key} ref or \"zone/key\" string.\n")
	b.WriteString("- attachToContext(ref) → string — load an S3 file as an image/file part so you can actually see it on the NEXT turn. `ref` is a {zone, key} object or a \"zone/key\" string. Idempotent per run. For text files (CSV, JSON, etc.) use storage_{zone}.get(...) instead.\n")
	b.WriteString("- transcribeAudio(ref, opts?) → {text, language?, duration?} — speech-to-text on a stored audio file. `ref` is a {zone, key} object or \"zone/key\" string. opts: {language?, prompt?, mimeType?}.\n")
	b.WriteString("- generateImage(prompt, opts?) → {file: {zone, key}, mimeType, size} — text-to-image; result auto-saved to S3, pass `file` to printToUser({source: file, ...}) or storage_{zone}.get(...). opts: {saveAs?, size?, aspectRatio?, seed?}.\n")
	b.WriteString("- speak(text, opts?) → {file: {zone, key}, mimeType, size} — text-to-speech; result auto-saved to S3, pass `file` to printToUser({source: file, type: 'audio'}). opts: {saveAs?, voice?, outputFormat?, speed?}.\n")
	b.WriteString("- embed(texts) → number[][] — text embeddings; accepts a string or array of strings.\n")
	if accessSatisfies(callerAccess, AccessAdmin) {
		b.WriteString("- requestUpgrade(description) → string — admin-only; ask Airlock to regenerate this agent with new capabilities. Not exposed to AccessUser / AccessPublic callers because the regenerate runs untrusted LLM-generated code on the build host.\n")
	}

	// Storage zone bindings — which storage_{slug} objects are actually
	// available, and on which axes (read/write). The framework
	// auto-registers a "tmp" zone, so this section is always non-empty.
	if len(agent.storages) > 0 {
		b.WriteString("\nStorage zones registered for this agent:\n")
		for slug, zone := range agent.storages {
			axes := ""
			switch {
			case zone.Read != AccessInternal && zone.Write != AccessInternal:
				axes = "read+write"
			case zone.Read != AccessInternal:
				axes = "read-only"
			case zone.Write != AccessInternal:
				axes = "write-only"
			default:
				axes = "internal"
			}
			desc := zone.Description
			if desc == "" && slug == reservedTmpSlug {
				desc = "framework scratch zone for transient files (httpRequest savedTo, generateImage results, etc.)"
			}
			if desc != "" {
				b.WriteString(fmt.Sprintf("- storage_%s (%s) — %s\n", slug, axes, desc))
			} else {
				b.WriteString(fmt.Sprintf("- storage_%s (%s)\n", slug, axes))
			}
		}
	}

	// Topic bindings.
	if len(agent.topics) > 0 {
		b.WriteString("\nNotification topics (subscribe the current conversation to receive notifications):\n")
		for slug, def := range agent.topics {
			b.WriteString(fmt.Sprintf("- topic_%s.subscribe() / topic_%s.unsubscribe() — %s\n", slug, slug, def.Description))
		}
	}

	// Connection LLM instructions.
	for slug, def := range agent.auths {
		if def.LLMHint == "" && len(def.Scopes) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("\nConnection %q (%s):\n", slug, def.Name))
		if def.AuthMode == "oauth" && len(def.Scopes) > 0 {
			b.WriteString(fmt.Sprintf("OAuth scopes: %s\n", strings.Join(def.Scopes, ", ")))
		}
		if def.LLMHint != "" {
			b.WriteString(def.LLMHint)
			b.WriteString("\n")
		}
	}

	return b.String()
}
