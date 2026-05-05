package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/tsrender"
	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/websearch"
	"github.com/dop251/goja"
)

// pathArg extracts a path string from a JS argument. Absolute unix paths
// only — anything else returns an error the binding turns into a JS throw.
func pathArg(val goja.Value) (string, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", fmt.Errorf("path is required")
	}
	s := val.String()
	if s == "" {
		return "", fmt.Errorf("path is required")
	}
	return s, nil
}

// fileInfoToJS shapes a FileInfo as a plain JS object the LLM can read.
func fileInfoToJS(vm *goja.Runtime, info FileInfo) goja.Value {
	obj := vm.NewObject()
	obj.Set("path", info.Path)
	if info.Filename != "" {
		obj.Set("filename", info.Filename)
	}
	if info.ContentType != "" {
		obj.Set("contentType", info.ContentType)
	}
	obj.Set("size", info.Size)
	if !info.LastModified.IsZero() {
		obj.Set("lastModified", info.LastModified.Format("2006-01-02T15:04:05Z"))
	}
	return obj
}

// mediaResultToJS shapes a *mediaResult as the JS-facing
// { file: FileInfo, mimeType, size } structure. The path inside the file
// object is what the LLM passes to readBytes / printToUser / attachToContext.
func mediaResultToJS(vm *goja.Runtime, res *mediaResult) goja.Value {
	file := vm.NewObject()
	file.Set("path", res.Path)
	file.Set("contentType", res.MimeType)
	file.Set("size", res.Size)
	out := vm.NewObject()
	out.Set("file", file)
	out.Set("mimeType", res.MimeType)
	out.Set("size", res.Size)
	return out
}

// newVM creates a fresh goja Runtime for a Run, binding all registered
// Go functions and built-in bindings.
func newVM(run *run, agent *Agent) *goja.Runtime {
	vm := goja.New()

	// Bind registered tools. Each RegisterTool(&Tool[In, Out]{...}) becomes
	// a typed JS global: JS input → json.Marshal → decode into In → typed
	// Execute → Out → json.Marshal → JSON.parse → JS value. Errors surface
	// as JS throws via vm.NewGoError. Tools whose declared Access exceeds
	// the run's callerAccess are simply not bound — invisible to the LLM.
	for _, t := range agent.tools {
		if !accessSatisfies(run.callerAccess, t.Access) {
			continue
		}
		t := t // capture
		vm.Set(t.Name, func(call goja.FunctionCall) goja.Value {
			var argJS any
			if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
				argJS = call.Arguments[0].Export()
			}
			raw, err := json.Marshal(argJS)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("%s: marshal args: %w", t.Name, err)))
			}
			ctx := contextWithRun(run.ctx, run)
			outJSON, err := t.Execute(ctx, raw)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			if outJSON == "" {
				return goja.Undefined()
			}
			var parsed any
			if err := json.Unmarshal([]byte(outJSON), &parsed); err != nil {
				return vm.ToValue(outJSON)
			}
			return vm.ToValue(parsed)
		})
	}

	// Inject persistent store for conversation-scoped runs.
	// Uses a native goja Object (not a Go map proxy) so JS function source
	// is preserved by toString() for serialization in teardownStore().
	if cvm := agent.getOrCreateConvVM(run.conversationID); cvm != nil {
		run.convVM = cvm
		cvm.mu.Lock()

		storeObj := vm.NewObject()

		// Populate from Go map (plain values).
		for key, val := range cvm.store {
			storeObj.Set(key, val)
		}

		// Re-evaluate serialized functions in the fresh VM.
		for key, src := range cvm.serializedFuncs {
			val, err := vm.RunString("(" + src + ")")
			if err == nil {
				storeObj.Set(key, val)
			}
			// If eval fails (e.g. closure referencing dead scope), silently drop.
		}

		vm.Set("store", storeObj)
		cvm.mu.Unlock()
	}

	// Register conn_{slug} objects for each registered connection.
	// Skip any whose required Access exceeds the run's callerAccess.
	for slug, conn := range run.agent.auths {
		if !accessSatisfies(run.callerAccess, conn.Access) {
			continue
		}
		handle := &ConnectionHandle{slug: slug, agent: run.agent}

		obj := vm.NewObject()
		connSlug := slug // capture for closures

		obj.Set("request", func(call goja.FunctionCall) goja.Value {
			method := call.Argument(0).String()
			path := call.Argument(1).String()
			var body any
			if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) {
				body = call.Arguments[2].String()
			}
			result, err := handle.Request(run.ctx, method, path, body)
			if err != nil {
				if ae, ok := IsAuthRequired(err); ok {
					errObj := vm.NewObject()
					errObj.Set("authRequired", true)
					errObj.Set("slug", ae.Slug)
					errObj.Set("connName", ae.ConnName)
					panic(vm.ToValue(errObj))
				}
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(string(result))
		})

		obj.Set("requestJSON", func(call goja.FunctionCall) goja.Value {
			method := call.Argument(0).String()
			path := call.Argument(1).String()
			var body any
			if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) {
				body = call.Arguments[2].Export()
			}
			raw, err := handle.Request(run.ctx, method, path, body)
			if err != nil {
				if ae, ok := IsAuthRequired(err); ok {
					errObj := vm.NewObject()
					errObj.Set("authRequired", true)
					errObj.Set("slug", ae.Slug)
					errObj.Set("connName", ae.ConnName)
					panic(vm.ToValue(errObj))
				}
				panic(vm.NewGoError(err))
			}
			var result any
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &result); err != nil {
					panic(vm.NewGoError(fmt.Errorf("decode JSON response: %w", err)))
				}
			}
			return vm.ToValue(result)
		})

		_ = connSlug // used for future error messages if needed
		vm.Set("conn_"+slug, obj)
	}

	// queryDB / execDB are admin-only. AccessUser / AccessPublic callers
	// shouldn't be able to coax the LLM into running arbitrary SQL — for
	// user-facing data access, builders register typed tools that wrap
	// queryDB internally and enforce row-level filtering. Gate is bind-time
	// so the bindings simply don't exist for non-admin runs and can't be
	// reached even via prompt injection.
	if accessSatisfies(run.callerAccess, AccessAdmin) {
		vm.Set("queryDB", func(call goja.FunctionCall) goja.Value {
			db := agent.DB()
			if db == nil {
				panic(vm.NewGoError(fmt.Errorf("agent database not configured (AIRLOCK_DB_URL not set)")))
			}
			query := call.Argument(0).String()
			params := make([]any, len(call.Arguments)-1)
			for i := 1; i < len(call.Arguments); i++ {
				params[i-1] = call.Arguments[i].Export()
			}
			rows, err := db.QueryContext(run.ctx, query, params...)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			defer rows.Close()
			result, err := rowsToMaps(rows)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			return vm.ToValue(result)
		})

		vm.Set("execDB", func(call goja.FunctionCall) goja.Value {
			db := agent.DB()
			if db == nil {
				panic(vm.NewGoError(fmt.Errorf("agent database not configured (AIRLOCK_DB_URL not set)")))
			}
			query := call.Argument(0).String()
			params := make([]any, len(call.Arguments)-1)
			for i := 1; i < len(call.Arguments); i++ {
				params[i-1] = call.Arguments[i].Export()
			}
			res, err := db.ExecContext(run.ctx, query, params...)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			affected, _ := res.RowsAffected()
			obj := vm.NewObject()
			obj.Set("rowsAffected", affected)
			return vm.ToValue(obj)
		})
	}

	// Flat unix-style file API. Every binding wraps run.ctx with the
	// run's caller, then routes through agent.CheckFileAccess (the single
	// gate for untrusted territory) before forwarding to the trusted
	// internal raw helpers. Builders that construct paths in their own Go
	// code call agent.OpenFile/ReadFile/WriteFile/StatFile/ListDir/
	// DeleteFile directly — those skip the check.
	//
	// Public-caller gating: each binding only appears in the JS runtime
	// when there's something the caller could plausibly do with it. For
	// AccessUser/AccessAdmin all bindings are present (per-call
	// CheckFileAccess still enforces directory caps). For AccessPublic
	// the binding only appears when at least one registered directory
	// grants AccessPublic for the matching op — otherwise the binding
	// would just throw on every call. Keeps the public attack surface
	// minimal and the public-caller's tool description honest.
	publicReadOK := agent.hasPublicDirCap(OpRead)
	publicWriteOK := agent.hasPublicDirCap(OpWrite)
	publicListOK := agent.hasPublicDirCap(OpList)
	authedFile := accessSatisfies(run.callerAccess, AccessUser)

	// readFile(path) → string — UTF-8 text. Most-common case. The
	// underlying bytes are decoded as UTF-8; non-text content surfaces
	// silently as a possibly-mangled string. For binary, use readBytes.
	if authedFile || publicReadOK {
		vm.Set("readFile", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("readFile: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("readFile: %w", err)))
			}
			b, err := agent.ReadFile(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("readFile: %w", err)))
			}
			return vm.ToValue(string(b))
		})

		// readBytes(path) → Uint8Array — binary content (images, PDFs, etc.).
		// We wrap the underlying ArrayBuffer in a Uint8Array so the standard
		// JS idioms work: indexed access, .length, iteration, .slice() that
		// returns a typed array, Array.from() to materialize a plain array.
		// A raw ArrayBuffer would force the LLM to write `new Uint8Array(ab)`
		// before doing anything useful, which it doesn't reliably remember.
		vm.Set("readBytes", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("readBytes: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("readBytes: %w", err)))
			}
			b, err := agent.ReadFile(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("readBytes: %w", err)))
			}
			ab := vm.NewArrayBuffer(b)
			// Uint8Array is a TypedArray constructor — must be invoked with
			// `new`. AssertFunction calls without `new` and TypedArrays
			// reject that; AssertConstructor is the right tool here.
			u8Ctor, ok := goja.AssertConstructor(vm.Get("Uint8Array"))
			if !ok {
				// Shouldn't happen — Uint8Array is a runtime built-in. Fall
				// back to the raw buffer rather than silently returning
				// nothing.
				return vm.ToValue(ab)
			}
			u8, err := u8Ctor(nil, vm.ToValue(ab))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("readBytes: wrap as Uint8Array: %w", err)))
			}
			return u8
		})
	}

	// writeFile(path, data, contentType?) → FileInfo
	if authedFile || publicWriteOK {
		vm.Set("writeFile", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("writeFile: %w", err)))
			}
			dataVal := call.Argument(1)
			if dataVal == nil || goja.IsUndefined(dataVal) || goja.IsNull(dataVal) {
				panic(vm.NewGoError(fmt.Errorf("writeFile: data is required")))
			}
			var data []byte
			switch v := dataVal.Export().(type) {
			case string:
				data = []byte(v)
			case []byte:
				data = v
			case goja.ArrayBuffer:
				data = v.Bytes()
			default:
				data = []byte(dataVal.String())
			}
			var contentType string
			if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
				contentType = call.Arguments[2].String()
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpWrite); err != nil {
				panic(vm.NewGoError(fmt.Errorf("writeFile: %w", err)))
			}
			info, err := agent.WriteFile(ctx, path, strings.NewReader(string(data)), contentType)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("writeFile: %w", err)))
			}
			return fileInfoToJS(vm, info)
		})

		// deleteFile(path) — folds into Write cap (write on the parent governs unlink).
		vm.Set("deleteFile", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("deleteFile: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpWrite); err != nil {
				panic(vm.NewGoError(fmt.Errorf("deleteFile: %w", err)))
			}
			if err := agent.DeleteFile(ctx, path); err != nil {
				panic(vm.NewGoError(fmt.Errorf("deleteFile: %w", err)))
			}
			return goja.Undefined()
		})
	}

	// listDir(path, opts?) → FileInfo[] — non-recursive by default.
	if authedFile || publicListOK {
		vm.Set("listDir", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("listDir: %w", err)))
			}
			opts := ListOpts{}
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				o := call.Arguments[1].ToObject(vm)
				if v := o.Get("recursive"); v != nil && !goja.IsUndefined(v) {
					opts.Recursive = v.ToBoolean()
				}
			}
			ctx := run.checkedCtx()
			// Strip trailing slash for the access check (a directory listing
			// usually ends with `/` but the gate compares paths).
			checkPath := strings.TrimRight(path, "/")
			if checkPath == "" {
				checkPath = path
			}
			if err := agent.CheckFileAccess(ctx, checkPath, OpList); err != nil {
				panic(vm.NewGoError(fmt.Errorf("listDir: %w", err)))
			}
			files, err := agent.ListDir(ctx, path, opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("listDir: %w", err)))
			}
			out := make([]goja.Value, len(files))
			for i, f := range files {
				out[i] = fileInfoToJS(vm, f)
			}
			return vm.ToValue(out)
		})
	}

	// statFile + fileExists + shareFileURL all gate on Read.
	if authedFile || publicReadOK {
		vm.Set("statFile", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("statFile: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("statFile: %w", err)))
			}
			info, err := agent.StatFile(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("statFile: %w", err)))
			}
			return fileInfoToJS(vm, info)
		})

		// fileExists(path) → bool — sugar around statFile.
		vm.Set("fileExists", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileExists: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				// Indistinguishable from "not found" by design.
				return vm.ToValue(false)
			}
			_, err = agent.StatFile(ctx, path)
			return vm.ToValue(err == nil)
		})

		// shareFileURL(path, opts?) → { url, expiresAtMs }
		// Returns a presigned, unauthenticated, time-limited URL for the
		// stored file. opts: { expiresInMinutes? } — server defaults to 1h
		// and caps at 24h. For embedding files in markdown links or sharing
		// outside the agent's auth boundary. To show a file in chat, prefer
		// printToUser({type:"file", source:path}) instead.
		vm.Set("shareFileURL", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("shareFileURL: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("shareFileURL: %w", err)))
			}
			ttl := time.Duration(0)
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				o := call.Arguments[1].ToObject(vm)
				if v := o.Get("expiresInMinutes"); v != nil && !goja.IsUndefined(v) {
					ttl = time.Duration(v.ToInteger()) * time.Minute
				}
			}
			resp, err := agent.ShareFileURL(ctx, path, ttl)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("shareFileURL: %w", err)))
			}
			out := vm.NewObject()
			out.Set("url", resp.URL)
			out.Set("expiresAtMs", resp.ExpiresAtMs)
			return out
		})
	}

	// printToUser(parts) — send rich content to the user's conversation.
	// Accepts a single DisplayPart object or an array of DisplayPart objects.
	vm.Set("printToUser", func(call goja.FunctionCall) goja.Value {
		parts := parseDisplayParts(vm, call.Argument(0))
		if err := run.printToUser(run.ctx, parts, ""); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	// Register topic_{slug} objects for each registered topic.
	// Skip any whose required Access exceeds the run's callerAccess.
	for slug, topic := range agent.topics {
		if !accessSatisfies(run.callerAccess, topic.Access) {
			continue
		}
		topicObj := vm.NewObject()
		topicSlug := slug // capture for closure

		topicObj.Set("subscribe", func(call goja.FunctionCall) goja.Value {
			if err := run.subscribeTopic(run.ctx, topicSlug); err != nil {
				panic(vm.NewGoError(err))
			}
			return goja.Undefined()
		})

		topicObj.Set("unsubscribe", func(call goja.FunctionCall) goja.Value {
			if err := run.unsubscribeTopic(run.ctx, topicSlug); err != nil {
				panic(vm.NewGoError(err))
			}
			return goja.Undefined()
		})

		vm.Set("topic_"+slug, topicObj)
	}

	makeLogFn := func(level LogLevel) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			// Match browser console.log: format every argument, join with
			// spaces. Single-arg calls (the common case) behave the same
			// as before; multi-arg calls like `log("desc:", value)` no
			// longer silently drop everything after the first arg.
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = formatJSValue(vm, a)
			}
			msg := strings.Join(parts, " ")
			run.logAppend(level, msg)
			run.mu.Lock()
			run.pendingLogs = append(run.pendingLogs, LogEntry{Level: level, Message: msg})
			run.mu.Unlock()
			return goja.Undefined()
		}
	}
	logFn := makeLogFn(LogLevelInfo)
	vm.Set("log", logFn)

	// Alias console.log/warn/error so LLMs that generate console.log() just
	// work. console.warn/error map to the matching LogLevel so severity is
	// preserved in the run timeline.
	console := vm.NewObject()
	console.Set("log", logFn)
	console.Set("warn", makeLogFn(LogLevelWarn))
	console.Set("error", makeLogFn(LogLevelError))
	vm.Set("console", console)

	// Authenticated-caller-only bindings: HTTP egress, web search, AI
	// helpers, attachment to LLM context. All of these consume metered
	// resources (token usage on LLM-backed helpers, outbound bandwidth /
	// rate limits on httpRequest+webSearch) so an unauthenticated public
	// caller must not drive them. Bind-time gate: bindings simply don't
	// exist in the JS runtime for AccessPublic — can't be coaxed into
	// existence by prompt injection.
	if accessSatisfies(run.callerAccess, AccessUser) {

		// HTTP requests via Airlock proxy.
		httpClient := &proxyHTTPClient{client: agent.client}
		vm.Set("httpRequest", func(call goja.FunctionCall) goja.Value {
			url := call.Argument(0).String()
			if url == "" {
				panic(vm.NewGoError(fmt.Errorf("httpRequest: url is required")))
			}

			req := HTTPRequest{URL: url, Method: "GET"}

			// Parse optional opts object.
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				opts := call.Arguments[1].ToObject(vm)
				if v := opts.Get("method"); v != nil && !goja.IsUndefined(v) {
					req.Method = v.String()
				}
				if v := opts.Get("headers"); v != nil && !goja.IsUndefined(v) {
					exported := v.Export()
					if m, ok := exported.(map[string]any); ok {
						req.Headers = make(map[string]string, len(m))
						for k, val := range m {
							req.Headers[k] = fmt.Sprintf("%v", val)
						}
					}
				}
				if v := opts.Get("body"); v != nil && !goja.IsUndefined(v) {
					if _, ok := v.Export().(string); ok {
						req.Body = v.String()
					} else {
						// Object/array → JSON serialize.
						b, err := json.Marshal(v.Export())
						if err != nil {
							panic(vm.NewGoError(fmt.Errorf("httpRequest: failed to serialize body: %w", err)))
						}
						req.Body = string(b)
						// Auto-set Content-Type if not explicitly provided.
						if req.Headers == nil {
							req.Headers = map[string]string{}
						}
						if _, hasContentType := req.Headers["Content-Type"]; !hasContentType {
							req.Headers["Content-Type"] = "application/json"
						}
					}
				}
				if v := opts.Get("timeout"); v != nil && !goja.IsUndefined(v) {
					req.Timeout = int(v.ToInteger())
				}
				if v := opts.Get("saveAs"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
					// saveAs is a storage path under a registered directory
					// the caller has Write access to. Airlock just writes
					// wherever we tell it; the gate lives here.
					path := v.String()
					if path == "" {
						panic(vm.NewGoError(fmt.Errorf("httpRequest: saveAs must be a non-empty storage path")))
					}
					if err := agent.CheckFileAccess(run.checkedCtx(), path, OpWrite); err != nil {
						panic(vm.NewGoError(fmt.Errorf("httpRequest: saveAs: %w", err)))
					}
					req.SaveAs = path
				}
				if v := opts.Get("raw"); v != nil && !goja.IsUndefined(v) {
					// raw: skip HTML→markdown conversion (default is to convert HTML).
					req.Raw = v.ToBoolean()
				}
			}

			resp, err := httpClient.Do(run.ctx, req)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("httpRequest: %w", err)))
			}

			result := vm.NewObject()
			result.Set("status", resp.Status)
			result.Set("headers", resp.Headers)
			result.Set("contentType", resp.ContentType)
			result.Set("size", resp.Size)
			if resp.SavedTo != "" {
				// Airlock returns the storage path; expose it as a string so
				// the LLM can pass it directly to readFile/readBytes/
				// attachToContext/etc.
				result.Set("savedTo", resp.SavedTo)
			}
			if resp.Note != "" {
				result.Set("note", resp.Note)
			}

			// Auto-parse JSON responses.
			if strings.Contains(resp.ContentType, "application/json") && resp.Body != "" {
				var parsed any
				if jsonErr := json.Unmarshal([]byte(resp.Body), &parsed); jsonErr == nil {
					result.Set("body", parsed)
					return result
				}
			}

			result.Set("body", resp.Body)
			return result
		})

		// Web search via Airlock proxy (no API keys in container).
		searchClient := &proxySearchClient{client: agent.client}
		vm.Set("webSearch", func(call goja.FunctionCall) goja.Value {
			query := call.Argument(0).String()
			count := 5
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				count = int(call.Arguments[1].ToInteger())
			}
			resp, err := searchClient.Search(run.ctx, websearch.Request{
				Query: query,
				Count: count,
			})
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("web search: %w", err)))
			}
			return vm.ToValue(resp)
		})

		// attachToContext(path) — load a stored file so the model can see it
		// on the next turn. `path` is a storage path; idempotent per run;
		// bytes are collected on run.pendingAttachments and drained into the
		// run_js tool.Result by buildRunJSTool.
		vm.Set("attachToContext", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("attachToContext: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("attachToContext: %w", err)))
			}

			// Idempotent: skip if already attached this run.
			run.mu.Lock()
			if run.attachedKeys == nil {
				run.attachedKeys = make(map[string]struct{})
			}
			if _, ok := run.attachedKeys[path]; ok {
				run.mu.Unlock()
				return vm.ToValue("Already in context for this turn.")
			}
			run.attachedKeys[path] = struct{}{}
			run.mu.Unlock()

			info, err := agent.StatFile(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("attachToContext: file not found: %w", err)))
			}

			if len(run.supportedModalities) > 0 && !mimeMatchesModalities(info.ContentType, run.supportedModalities) {
				panic(vm.NewGoError(fmt.Errorf(
					"attachToContext: %s files are not supported by the current model. Supported types: %s. Use readFile(path) for text-based files",
					info.ContentType, strings.Join(run.supportedModalities, ", "))))
			}

			// Emit a s3ref: sentinel rather than loading + base64-encoding here.
			// Airlock's attachref resolver picks this up on the LLM-stream and
			// session-append paths, canonicalizes to llm/agents/<id>/<path>,
			// and either presigns a URL or inlines base64. The sentinel carries
			// the storage path so the resolver can presign the right S3 object.
			run.mu.Lock()
			run.pendingAttachments = append(run.pendingAttachments, tool.Attachment{
				Data:     "s3ref:" + path,
				MimeType: info.ContentType,
				Filename: pathBase(path),
			})
			run.mu.Unlock()

			return vm.ToValue(fmt.Sprintf("Attached %s (%s). The file is visible on the next turn.", path, info.ContentType))
		})

		// analyzeImage(path, question?) → string
		// Loads bytes from agent storage, sends them to the vision-capability
		// LLM with the (optional) question, and returns the model's reply.
		// Capability-routed: airlock picks the agent's vision_model (or system
		// default) regardless of which exec model the agent's main run uses.
		vm.Set("analyzeImage", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("analyzeImage: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("analyzeImage: %w", err)))
			}
			var question string
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				question = call.Arguments[1].String()
			}
			text, err := run.analyzeImage(ctx, path, question)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("analyzeImage: %w", err)))
			}
			return vm.ToValue(text)
		})

		// transcribeAudio(path, opts?) → { text, language?, duration? }
		// Loads bytes from agent storage, runs through the system-default STT
		// model, returns the transcript. Capability-routed: no slug needed.
		vm.Set("transcribeAudio", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("transcribeAudio: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("transcribeAudio: %w", err)))
			}
			var opts model.TranscribeCallOptions
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				o := call.Arguments[1].ToObject(vm)
				if v := o.Get("language"); v != nil && !goja.IsUndefined(v) {
					opts.Language = v.String()
				}
				if v := o.Get("prompt"); v != nil && !goja.IsUndefined(v) {
					opts.Prompt = v.String()
				}
				if v := o.Get("mimeType"); v != nil && !goja.IsUndefined(v) {
					opts.MimeType = v.String()
				}
			}
			res, err := run.transcribeAudio(ctx, path, opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("transcribeAudio: %w", err)))
			}
			out := vm.NewObject()
			out.Set("text", res.Text)
			if res.Language != "" {
				out.Set("language", res.Language)
			}
			if res.Duration != nil {
				out.Set("duration", *res.Duration)
			}
			return out
		})

		// generateImage(prompt, opts?) → { file: FileInfo, mimeType, size }
		// opts: { saveAs?, size?, aspectRatio?, seed? } — saveAs is a
		// storage path under a registered directory; omitted writes to "tmp"
		// with an auto-generated filename.
		vm.Set("generateImage", func(call goja.FunctionCall) goja.Value {
			prompt := call.Argument(0).String()
			if prompt == "" {
				panic(vm.NewGoError(fmt.Errorf("generateImage: prompt is required")))
			}
			var saveAs string
			var opts model.ImageCallOptions
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				o := call.Arguments[1].ToObject(vm)
				if v := o.Get("saveAs"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
					saveAs = v.String()
					if saveAs == "" {
						panic(vm.NewGoError(fmt.Errorf("generateImage: saveAs must be a non-empty storage path")))
					}
					if err := agent.CheckFileAccess(run.checkedCtx(), saveAs, OpWrite); err != nil {
						panic(vm.NewGoError(fmt.Errorf("generateImage: saveAs: %w", err)))
					}
				}
				if v := o.Get("size"); v != nil && !goja.IsUndefined(v) {
					opts.Size = v.String()
				}
				if v := o.Get("aspectRatio"); v != nil && !goja.IsUndefined(v) {
					opts.AspectRatio = v.String()
				}
				if v := o.Get("seed"); v != nil && !goja.IsUndefined(v) {
					s := v.ToInteger()
					opts.Seed = &s
				}
			}
			res, err := run.generateImage(run.ctx, prompt, saveAs, opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("generateImage: %w", err)))
			}
			return mediaResultToJS(vm, res)
		})

		// speak(text, opts?) → { file: FileInfo, mimeType, size }
		// opts: { saveAs?, voice?, outputFormat?, speed? } — saveAs is a
		// storage path under a registered directory.
		vm.Set("speak", func(call goja.FunctionCall) goja.Value {
			text := call.Argument(0).String()
			if text == "" {
				panic(vm.NewGoError(fmt.Errorf("speak: text is required")))
			}
			var saveAs string
			var opts model.SpeechCallOptions
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				o := call.Arguments[1].ToObject(vm)
				if v := o.Get("saveAs"); v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
					saveAs = v.String()
					if saveAs == "" {
						panic(vm.NewGoError(fmt.Errorf("speak: saveAs must be a non-empty storage path")))
					}
					if err := agent.CheckFileAccess(run.checkedCtx(), saveAs, OpWrite); err != nil {
						panic(vm.NewGoError(fmt.Errorf("speak: saveAs: %w", err)))
					}
				}
				if v := o.Get("voice"); v != nil && !goja.IsUndefined(v) {
					opts.Voice = v.String()
				}
				if v := o.Get("outputFormat"); v != nil && !goja.IsUndefined(v) {
					opts.OutputFormat = v.String()
				}
				if v := o.Get("speed"); v != nil && !goja.IsUndefined(v) {
					s := v.ToFloat()
					opts.Speed = &s
				}
			}
			res, err := run.generateSpeech(run.ctx, text, saveAs, opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("speak: %w", err)))
			}
			return mediaResultToJS(vm, res)
		})

		// embed(texts) → number[][]
		// Accepts a single string or an array of strings.
		vm.Set("embed", func(call goja.FunctionCall) goja.Value {
			arg := call.Argument(0)
			if arg == nil || goja.IsUndefined(arg) || goja.IsNull(arg) {
				panic(vm.NewGoError(fmt.Errorf("embed: texts is required")))
			}
			var texts []string
			switch v := arg.Export().(type) {
			case string:
				texts = []string{v}
			case []any:
				texts = make([]string, 0, len(v))
				for _, x := range v {
					if s, ok := x.(string); ok {
						texts = append(texts, s)
					}
				}
			default:
				panic(vm.NewGoError(fmt.Errorf("embed: texts must be a string or an array of strings")))
			}
			if len(texts) == 0 {
				panic(vm.NewGoError(fmt.Errorf("embed: at least one text is required")))
			}
			out, err := run.embed(run.ctx, texts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("embed: %w", err)))
			}
			return vm.ToValue(out)
		})

	} // end if accessSatisfies(AccessUser) — authenticated-only bindings

	// requestUpgrade(description) — ask Airlock to regenerate this agent with
	// new capabilities. The current agent keeps running until the new build
	// finishes. Admin-only because the rebuild runs untrusted LLM-generated
	// Go code on the agent-builder host with broad system access; non-admin
	// callers must not be able to trigger that. Gated at bind time so the
	// LLM's run_js environment for AccessUser/AccessPublic runs simply
	// doesn't have the binding (and the tool description below also omits
	// it for those callers).
	if accessSatisfies(run.callerAccess, AccessAdmin) {
		vm.Set("requestUpgrade", func(call goja.FunctionCall) goja.Value {
			description := call.Argument(0).String()
			if description == "" {
				panic(vm.NewGoError(fmt.Errorf("requestUpgrade: description is required")))
			}
			body := struct {
				Description    string `json:"description"`
				ConversationID string `json:"conversationId,omitempty"`
			}{description, run.conversationID}
			if err := agent.client.doJSON(run.ctx, "POST", "/api/agent/upgrade", body, nil); err != nil {
				panic(vm.NewGoError(fmt.Errorf("requestUpgrade: %w", err)))
			}
			return vm.ToValue("Upgrade requested. The agent will be regenerated in the background.")
		})
	}

	// Register mcp_{slug} objects for each registered MCP server.
	// Schemas come from Airlock via SyncResponse and live in agent.mcpSchemas;
	// we snapshot once per VM build so /refresh writes during a run can't
	// mutate the map mid-iteration. The LLM sees one typed method per
	// discovered tool — schemas already appear as declarations in the
	// per-run env prompt, so within a run there's nothing further to
	// discover.
	mcpSchemas := run.agent.snapshotMCPSchemas()
	for slug, mcp := range run.agent.mcps {
		if !accessSatisfies(run.callerAccess, mcp.Access) {
			continue
		}
		handle := &MCPHandle{slug: slug, agent: run.agent}

		obj := vm.NewObject()
		mcpSlug := slug // capture for closures

		schemas := mcpSchemas[slug]
		names := make([]string, len(schemas))
		for i, schema := range schemas {
			names[i] = schema.Name
		}
		jsNames := tsrender.JSToolNames(names)
		for _, schema := range schemas {
			toolName := schema.Name
			obj.Set(jsNames[toolName], func(call goja.FunctionCall) goja.Value {
				return invokeMCPTool(vm, run.ctx, handle, mcpSlug, toolName, call.Argument(0))
			})
		}

		vm.Set("mcp_"+slug, obj)
	}

	return vm
}

// invokeMCPTool runs an MCP tool call from a JS binding and translates the
// result back to a JS value. Shared between the typed per-tool methods and
// the stringly-typed callTool fallback.
func invokeMCPTool(vm *goja.Runtime, ctx context.Context, handle *MCPHandle, mcpSlug, toolName string, argsArg goja.Value) goja.Value {
	var args any
	if argsArg != nil && !goja.IsUndefined(argsArg) && !goja.IsNull(argsArg) {
		args = argsArg.Export()
	}
	resp, err := handle.CallTool(ctx, toolName, args)
	if err != nil {
		if ae, ok := IsAuthRequired(err); ok {
			errObj := vm.NewObject()
			errObj.Set("authRequired", true)
			errObj.Set("slug", ae.Slug)
			errObj.Set("serverName", mcpSlug)
			panic(vm.ToValue(errObj))
		}
		panic(vm.NewGoError(fmt.Errorf("mcp_%s.%s: %w", mcpSlug, toolName, err)))
	}
	var out strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			out.WriteString(c.Text)
		}
	}
	raw := out.String()
	var content any = raw
	var parsed any
	if jsonErr := json.Unmarshal([]byte(raw), &parsed); jsonErr == nil {
		content = parsed
	}
	if resp.IsError {
		msg := "MCP tool error"
		if len(resp.Content) > 0 && resp.Content[0].Text != "" {
			msg = resp.Content[0].Text
		}
		errInst, _ := vm.New(vm.Get("Error"), vm.ToValue(msg))
		errInst.Set("isError", true)
		errInst.Set("content", content)
		panic(vm.ToValue(errInst))
	}
	return vm.ToValue(content)
}

// parseDisplayParts converts a goja Value to []DisplayPart.
// Accepts a single object or an array of objects.
func parseDisplayParts(vm *goja.Runtime, val goja.Value) []DisplayPart {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil
	}

	raw := val.Export()

	// Single object → wrap in slice.
	obj, isMap := raw.(map[string]any)
	if isMap {
		return []DisplayPart{mapToDisplayPart(obj)}
	}

	// Array of objects.
	arr, isArr := raw.([]any)
	if !isArr {
		return nil
	}
	parts := make([]DisplayPart, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			parts = append(parts, mapToDisplayPart(m))
		}
	}
	return parts
}

// mapToDisplayPart converts a JS object (map[string]any) to a DisplayPart.
func mapToDisplayPart(m map[string]any) DisplayPart {
	p := DisplayPart{}
	if v, ok := m["type"].(string); ok {
		p.Type = v
	}
	if v, ok := m["text"].(string); ok {
		p.Text = v
	}
	// `source` accepts a composed "zone/key" string or a {zone, key} ref
	// object (the shape returned by tool/savedTo results). Both normalize
	// to the composed form that DisplayPart.Source carries.
	if raw, ok := m["source"]; ok && raw != nil {
		switch v := raw.(type) {
		case string:
			p.Source = v
		case map[string]any:
			zone, _ := v["zone"].(string)
			key, _ := v["key"].(string)
			if zone != "" && key != "" {
				p.Source = zone + "/" + key
			}
		}
	}
	if v, ok := m["url"].(string); ok {
		p.URL = v
	}
	if v, ok := m["filename"].(string); ok {
		p.Filename = v
	}
	if v, ok := m["mimeType"].(string); ok {
		p.MimeType = v
	}
	if v, ok := m["alt"].(string); ok {
		p.Alt = v
	}
	if v, ok := m["duration"].(float64); ok {
		p.Duration = v
	}
	// Handle "data" — can be string (base64), []byte, or ArrayBuffer.
	if raw, ok := m["data"]; ok && raw != nil {
		switch d := raw.(type) {
		case []byte:
			p.Data = d
		case string:
			p.Data = []byte(d)
		case goja.ArrayBuffer:
			p.Data = d.Bytes()
		}
	}
	return p
}

// rowsToMaps converts sql.Rows to []map[string]any for JS consumption.
func rowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := values[i]
			// Convert []byte to string for JS friendliness.
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// executeJS runs JavaScript code in a goja VM and returns the result as a string.
// Catches panics and returns them as errors.
func executeJS(vm *goja.Runtime, code string) (string, error) {
	v, err := vm.RunString(code)
	if err != nil {
		return "", err
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "", nil
	}
	return formatJSValue(vm, v), nil
}

// formatJSValue converts a goja value to a readable string.
// Objects/arrays are JSON.stringified (like browser console).
func formatJSValue(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if obj, ok := v.(*goja.Object); ok {
		if stringify, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify")); ok {
			indent := vm.ToValue("  ")
			result, err := stringify(goja.Undefined(), obj, goja.Undefined(), indent)
			if err == nil && result != nil && !goja.IsUndefined(result) {
				return result.String()
			}
		}
	}
	return v.String()
}
