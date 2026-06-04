package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/tsrender"
	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/websearch"
	"github.com/dop251/goja"
	"github.com/google/uuid"
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
// object is what the LLM passes to fileReadBytes / output / attachToContext.
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
// maxJSCallStackSize caps run_js recursion depth. goja's default is
// math.MaxInt32 (effectively unbounded) and its call stack is heap-
// allocated, so unbounded recursion (e.g. `function f(x){return f(x+1)}
// f(0)`) doesn't hit a Go stack limit — it grows the heap until the run
// times out. Capping it makes runaway recursion fail fast with a
// *StackOverflowError that propagates out of RunString as a normal tool
// error. 10000 is deep enough for legitimate recursive agent code while
// catching infinite recursion in milliseconds.
const maxJSCallStackSize = 10000

// maxJSValueBytes caps how many bytes a single Go→JS boundary crossing may
// pull into the goja heap (a tool return, fileRead, fileReadBytes, MCP/A2A
// result). goja has no heap accounting, so a script that slurps a giant S3
// object (`fileRead(huge)`) or accumulates large tool results would grow the
// Go heap unbounded with near-zero instruction cost — invisible to any
// CPU/recursion guard. Capping each crossing makes a single oversized slurp
// fail fast with a clear, actionable error, and bounds per-iteration heap
// growth. Legit large payloads must be processed via a storage path
// (ranged/streamed), not loaded whole.
const maxJSValueBytes = 64 << 20 // 64 MiB

// maxReadFileBytes caps how much a whole-file read (fileRead/fileReadBytes)
// may pull into the goja heap. Lower than maxJSValueBytes because these slurp
// an entire object: over this, the binding errors and points the LLM at the
// streaming alternatives (fileGrep/fileHead/fileTail/fileLines/
// fileReadRangeBytes) or a file-to-file transform, none of which materialize
// the whole file.
const maxReadFileBytes = 16 << 20 // 16 MiB

// readCappedForJS reads rc fully but fails (with a signpost to the streaming
// alternatives) once it exceeds maxReadFileBytes. Always closes rc.
func readCappedForJS(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxReadFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReadFileBytes {
		return nil, fmt.Errorf(
			"file exceeds the %d MiB in-memory cap — scan it with fileGrep/fileHead/fileTail/fileLines, read a byte window with fileReadRangeBytes, or transform it file-to-file (fileDecode/fileDecodeText) instead of loading it whole",
			maxReadFileBytes>>20)
	}
	return b, nil
}

// bytesToUint8Array wraps b in a JS Uint8Array so the standard JS idioms work
// (indexed access, .length, iteration, .slice, Array.from). A raw ArrayBuffer
// would force the LLM to write `new Uint8Array(ab)` first, which it doesn't
// reliably remember.
func bytesToUint8Array(vm *goja.Runtime, b []byte) goja.Value {
	ab := vm.NewArrayBuffer(b)
	// Uint8Array is a TypedArray constructor — must be invoked with `new`.
	// AssertFunction calls without `new` and TypedArrays reject that;
	// AssertConstructor is the right tool here.
	u8Ctor, ok := goja.AssertConstructor(vm.Get("Uint8Array"))
	if !ok {
		// Shouldn't happen — Uint8Array is a runtime built-in. Fall back to
		// the raw buffer rather than silently returning nothing.
		return vm.ToValue(ab)
	}
	u8, err := u8Ctor(nil, vm.ToValue(ab))
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("wrap as Uint8Array: %w", err)))
	}
	return u8
}

// capJSBytes aborts the in-flight JS call (as a thrown error) when a value
// crossing the Go→JS boundary exceeds maxJSValueBytes. what names the source
// for the error message (e.g. "fileRead", a tool name).
func capJSBytes(vm *goja.Runtime, what string, n int) {
	if n > maxJSValueBytes {
		panic(vm.NewGoError(fmt.Errorf(
			"%s: result too large for run_js: %d bytes exceeds the %d MiB in-memory cap — process it via a storage path (read in ranges / stream) instead of loading it whole",
			what, n, maxJSValueBytes>>20)))
	}
}

func newVM(run *run, agent *Agent) *goja.Runtime {
	vm := goja.New()
	vm.SetMaxCallStackSize(maxJSCallStackSize)
	installAmplifierGuards(vm)

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
			run.gw.enter()
			outJSON, err := t.Execute(run.checkedCtx(), raw)
			run.gw.exit()
			if err != nil {
				panic(vm.NewGoError(err))
			}
			capJSBytes(vm, t.Name, len(outJSON))
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
			headers := headersFromJSArg(call.Argument(3))
			resp, err := handle.RequestStream(run.ctx, RequestOpts{
				Method: method, Path: path, Body: body, Headers: headers,
			})
			if err != nil {
				panic(vm.NewGoError(connectionErrorForJS(connSlug, err)))
			}
			defer resp.Body.Close()

			contentType := resp.Headers.Get("Content-Type")
			dst := fmt.Sprintf("tmp/conn-%s-%s.bin", connSlug, newCallID())
			inline, savedTo, size, err := peekAndSpill(run.ctx, run.agent, resp.Body, dst, contentType)
			if err != nil {
				panic(vm.NewGoError(connectionErrorForJS(connSlug, err)))
			}

			out := vm.NewObject()
			out.Set("status", resp.StatusCode)
			out.Set("contentType", contentType)
			out.Set("size", size)
			if savedTo == "" {
				out.Set("body", string(inline))
			} else {
				out.Set("bodyPreview", string(inline))
				out.Set("bodySavedTo", savedTo)
				out.Set("note", fmt.Sprintf("Body (%d bytes) exceeded inline threshold; saved to %s. Use fileRead(bodySavedTo) to read the full body.", size, savedTo))
			}
			return out
		})

		obj.Set("requestJSON", func(call goja.FunctionCall) goja.Value {
			method := call.Argument(0).String()
			path := call.Argument(1).String()
			var body any
			if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) {
				body = call.Arguments[2].Export()
			}
			headers := headersFromJSArg(call.Argument(3))
			resp, err := handle.RequestStream(run.ctx, RequestOpts{
				Method: method, Path: path, Body: body, Headers: headers,
			})
			if err != nil {
				panic(vm.NewGoError(connectionErrorForJS(connSlug, err)))
			}
			defer resp.Body.Close()

			contentType := resp.Headers.Get("Content-Type")
			dst := fmt.Sprintf("tmp/conn-%s-%s.bin", connSlug, newCallID())
			inline, savedTo, size, err := peekAndSpill(run.ctx, run.agent, resp.Body, dst, contentType)
			if err != nil {
				panic(vm.NewGoError(connectionErrorForJS(connSlug, err)))
			}

			out := vm.NewObject()
			out.Set("status", resp.StatusCode)
			out.Set("contentType", contentType)
			out.Set("size", size)
			if savedTo == "" {
				if len(inline) == 0 {
					out.Set("data", goja.Null())
					return out
				}
				// Round-trip through the runtime's own JSON.parse so the
				// resulting value is a native JS object (not a Go-reflected
				// map). Native objects stringify cleanly via JSON.stringify
				// and behave like real objects under Object.keys / spread /
				// in checks; Go-reflected wrappers don't, and toString them
				// as "[object Object]".
				parseFn, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
				if !ok {
					panic(vm.NewGoError(fmt.Errorf("requestJSON: JSON.parse not available")))
				}
				val, err := parseFn(goja.Undefined(), vm.ToValue(string(inline)))
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("decode JSON response: %w", err)))
				}
				out.Set("data", val)
				return out
			}
			out.Set("bodyPreview", string(inline))
			out.Set("bodySavedTo", savedTo)
			out.Set("note", fmt.Sprintf("Body (%d bytes) exceeded inline threshold; saved to %s. Read with: JSON.parse(fileRead(bodySavedTo)).", size, savedTo))
			return out
		})

		vm.Set("conn_"+slug, obj)
	}

	// Register exec_{slug}.run for each registered exec endpoint. Bind-time
	// gated by Access so non-admin runs never see admin-only exec endpoints
	// (AccessPublic is already demoted to AccessUser at registration).
	// JS bindings use RunStream so stdout/stderr above spillInlineThreshold
	// spill to tmp/ storage rather than blowing the tool-output cap.
	for slug, ep := range run.agent.execEndpoints {
		if !accessSatisfies(run.callerAccess, ep.Access) {
			continue
		}
		handle := &ExecHandle{slug: slug, agent: run.agent}
		obj := vm.NewObject()
		epSlug := slug // capture for closures

		obj.Set("run", func(call goja.FunctionCall) goja.Value {
			cmd := ExecCommand{}
			if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) {
				cmd.Command = call.Argument(0).String()
			}
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
				if exported, ok := call.Arguments[1].Export().([]any); ok {
					cmd.Args = make([]string, 0, len(exported))
					for _, a := range exported {
						cmd.Args = append(cmd.Args, fmt.Sprintf("%v", a))
					}
				} else {
					panic(vm.NewGoError(fmt.Errorf("exec_%s.run: args must be an array of strings", epSlug)))
				}
			}
			if len(call.Arguments) > 2 && !goja.IsUndefined(call.Arguments[2]) && !goja.IsNull(call.Arguments[2]) {
				opts, ok := call.Arguments[2].Export().(map[string]any)
				if !ok {
					panic(vm.NewGoError(fmt.Errorf("exec_%s.run: opts must be an object", epSlug)))
				}
				if v, ok := opts["stdin"]; ok && v != nil {
					cmd.Stdin = []byte(fmt.Sprintf("%v", v))
				}
				if v, ok := opts["timeoutMs"]; ok && v != nil {
					switch t := v.(type) {
					case int64:
						cmd.Timeout = time.Duration(t) * time.Millisecond
					case float64:
						cmd.Timeout = time.Duration(int64(t)) * time.Millisecond
					}
				}
			}

			stream, err := handle.RunStream(run.ctx, cmd)
			if err != nil {
				panic(vm.NewGoError(err))
			}
			defer stream.Stdout.Close()
			defer stream.Stderr.Close()

			// Both pipes must drain in parallel — sequential reads deadlock
			// because back-pressure on the unread pipe stalls airlock's
			// session-output goroutine.
			callID := newCallID()
			prefix := fmt.Sprintf("tmp/exec-%s-%s", epSlug, callID)
			outCh := make(chan spillFields, 1)
			errCh := make(chan spillFields, 1)
			go func() {
				outCh <- spillFor(run.ctx, run.agent, stream.Stdout, prefix+"-stdout.bin")
			}()
			go func() {
				errCh <- spillFor(run.ctx, run.agent, stream.Stderr, prefix+"-stderr.bin")
			}()
			outR := <-outCh
			errR := <-errCh
			exit, waitErr := stream.Wait()

			switch {
			case outR.err != nil:
				panic(vm.NewGoError(outR.err))
			case errR.err != nil:
				panic(vm.NewGoError(errR.err))
			case waitErr != nil:
				panic(vm.NewGoError(waitErr))
			}

			out := vm.NewObject()
			out.Set("exitCode", exit.ExitCode)
			out.Set("durationMs", exit.DurationMs)
			setStreamFields(out, "stdout", outR)
			setStreamFields(out, "stderr", errR)
			if outR.savedTo != "" || errR.savedTo != "" {
				out.Set("note", execOverflowNote(outR, errR))
			}
			return out
		})

		vm.Set("exec_"+slug, obj)
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

	// fileRead(path) → string — UTF-8 text. Most-common case. The
	// underlying bytes are decoded as UTF-8; non-text content surfaces
	// silently as a possibly-mangled string. For binary, use fileReadBytes.
	if authedFile || publicReadOK {
		vm.Set("fileRead", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileRead: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileRead: %w", err)))
			}
			rc, err := run.openCached(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileRead: %w", err)))
			}
			b, err := readCappedForJS(rc)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileRead: %w", err)))
			}
			return vm.ToValue(string(b))
		})

		// fileReadBytes(path) → Uint8Array — binary content (images, PDFs, etc.).
		// We wrap the underlying ArrayBuffer in a Uint8Array so the standard
		// JS idioms work: indexed access, .length, iteration, .slice() that
		// returns a typed array, Array.from() to materialize a plain array.
		// A raw ArrayBuffer would force the LLM to write `new Uint8Array(ab)`
		// before doing anything useful, which it doesn't reliably remember.
		vm.Set("fileReadBytes", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadBytes: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadBytes: %w", err)))
			}
			rc, err := run.openCached(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadBytes: %w", err)))
			}
			b, err := readCappedForJS(rc)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadBytes: %w", err)))
			}
			return bytesToUint8Array(vm, b)
		})

		// fileReadRangeBytes(path, start, length) → Uint8Array. Reads an exact
		// byte window without materializing the whole file (no charset
		// assumption — text random-access goes through the line ops instead).
		// Cache-aware: a locally-cached file is seeked; an uncached one is
		// fetched with a true S3 Range request. Capped at the fileRead limit.
		vm.Set("fileReadRangeBytes", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadRangeBytes: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadRangeBytes: %w", err)))
			}
			b, err := run.readRange(ctx, path, call.Argument(1).ToInteger(), call.Argument(2).ToInteger())
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileReadRangeBytes: %w", err)))
			}
			return bytesToUint8Array(vm, b)
		})

		// grep(path, pattern, opts?) → string of matching lines. opts:
		// { ignoreCase?, invert?, lineNumbers?, max? }. Streams the file line
		// by line; output is bounded and reports how many matches were
		// dropped past the cap.
		vm.Set("fileGrep", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileGrep: %w", err)))
			}
			patArg := call.Argument(1)
			if goja.IsUndefined(patArg) || goja.IsNull(patArg) {
				panic(vm.NewGoError(errors.New("fileGrep: pattern is required")))
			}
			opts := grepOpts{}
			if o := call.Argument(2); !goja.IsUndefined(o) && !goja.IsNull(o) {
				obj := o.ToObject(vm)
				if v := obj.Get("ignoreCase"); v != nil && !goja.IsUndefined(v) {
					opts.ignoreCase = v.ToBoolean()
				}
				if v := obj.Get("invert"); v != nil && !goja.IsUndefined(v) {
					opts.invert = v.ToBoolean()
				}
				if v := obj.Get("lineNumbers"); v != nil && !goja.IsUndefined(v) {
					opts.lineNumbers = v.ToBoolean()
				}
				if v := obj.Get("max"); v != nil && !goja.IsUndefined(v) {
					opts.max = int(v.ToInteger())
				}
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileGrep: %w", err)))
			}
			out, err := run.grepFile(ctx, path, patArg.String(), opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileGrep: %w", err)))
			}
			return vm.ToValue(out)
		})

		// head(path, n?) / tail(path, n?) → string (first / last n lines,
		// default 10). tail fetches only the trailing window of the file.
		vm.Set("fileHead", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileHead: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileHead: %w", err)))
			}
			out, err := run.headLines(ctx, path, int(call.Argument(1).ToInteger()))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileHead: %w", err)))
			}
			return vm.ToValue(out)
		})

		vm.Set("fileTail", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileTail: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileTail: %w", err)))
			}
			out, err := run.tailLines(ctx, path, int(call.Argument(1).ToInteger()))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileTail: %w", err)))
			}
			return vm.ToValue(out)
		})

		// fileLines(path, start, count) → string — a line window starting at
		// the 1-based line `start` (default 1) for `count` lines (default 10).
		vm.Set("fileLines", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileLines: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileLines: %w", err)))
			}
			out, err := run.readLineWindow(ctx, path, int(call.Argument(1).ToInteger()), int(call.Argument(2).ToInteger()))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileLines: %w", err)))
			}
			return vm.ToValue(out)
		})
	}

	// File→file transforms (authed only — a compound read+write power tool).
	// All run cache-aware and stream src→codec→output. `dst` is optional: omit
	// it for an auto scratch path (small text result rides back inline as
	// `content`, otherwise `savedTo`+`preview`+`size`); give it to write a
	// specific path (returns `savedTo`+`size`). dst must differ from src.
	if authedFile {
		// encodeFile(src, codec, dst?) / decodeFile(src, codec, dst?) — codec
		// is base64 | base64url | hex | gzip.
		vm.Set("fileEncode", func(call goja.FunctionCall) goja.Value {
			src, codecName, dst := transformArgs(vm, call, "fileEncode")
			fn, ok := encoders[codecName]
			if !ok {
				panic(vm.NewGoError(fmt.Errorf("fileEncode: unknown codec %q (base64, base64url, hex, gzip)", codecName)))
			}
			ctx := run.checkedCtx()
			checkTransformAccess(ctx, agent, vm, "fileEncode", src, dst)
			res, err := run.transformFile(ctx, src, codecName, dst, encodeContentType(codecName), codecSuffix[codecName], textCodecs[codecName], fn)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileEncode: %w", err)))
			}
			return transformResultToJS(vm, res)
		})

		vm.Set("fileDecode", func(call goja.FunctionCall) goja.Value {
			src, codecName, dst := transformArgs(vm, call, "fileDecode")
			fn, ok := decoders[codecName]
			if !ok {
				panic(vm.NewGoError(fmt.Errorf("fileDecode: unknown codec %q (base64, base64url, hex, gzip)", codecName)))
			}
			ctx := run.checkedCtx()
			checkTransformAccess(ctx, agent, vm, "fileDecode", src, dst)
			res, err := run.transformFile(ctx, src, codecName, dst, "application/octet-stream", ".bin", false, fn)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDecode: %w", err)))
			}
			return transformResultToJS(vm, res)
		})

		// decodeTextFile(src, charset, dst?) — decode charset bytes (latin1,
		// utf-16, …) to UTF-8 text.
		vm.Set("fileDecodeText", func(call goja.FunctionCall) goja.Value {
			src, charset, dst := transformArgs(vm, call, "fileDecodeText")
			fn, err := lookupCharset(charset)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDecodeText: %w", err)))
			}
			ctx := run.checkedCtx()
			checkTransformAccess(ctx, agent, vm, "fileDecodeText", src, dst)
			res, err := run.transformFile(ctx, src, charset, dst, "text/plain; charset=utf-8", ".txt", true, fn)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDecodeText: %w", err)))
			}
			return transformResultToJS(vm, res)
		})

		// Streaming editors — big-file-safe (rewrite only the changed region).
		// `dst` optional: omit → new scratch path (src untouched); pass a path
		// (may equal src for in-place) to write there.

		// fileEditLines(src, edits, dst?) — structured, 1-based line-addressed
		// edits: {from,count,text} (replace) · {from,count} (delete) ·
		// {from,count:0,text} (insert before) · {append:text}.
		vm.Set("fileEditLines", func(call goja.FunctionCall) goja.Value {
			src, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileEditLines: %w", err)))
			}
			edits, err := parseLineEdits(vm, call.Argument(1))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileEditLines: %w", err)))
			}
			dst := optPathArg(vm, call.Argument(2), "fileEditLines")
			ctx := run.checkedCtx()
			checkTransformAccess(ctx, agent, vm, "fileEditLines", src, dst)
			res, err := run.editLines(ctx, src, dst, edits)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileEditLines: %w", err)))
			}
			return transformResultToJS(vm, res)
		})

		// fileSed(src, script, dst?) — a sed subset: addresses N · N,M ·
		// /regex/ · $; commands s/re/repl/[gi] · d · c\text · i\text · a\text.
		// Replacement backrefs use Go syntax ($1).
		vm.Set("fileSed", func(call goja.FunctionCall) goja.Value {
			src, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileSed: %w", err)))
			}
			scriptArg := call.Argument(1)
			if goja.IsUndefined(scriptArg) || goja.IsNull(scriptArg) {
				panic(vm.NewGoError(errors.New("fileSed: script is required")))
			}
			dst := optPathArg(vm, call.Argument(2), "fileSed")
			ctx := run.checkedCtx()
			checkTransformAccess(ctx, agent, vm, "fileSed", src, dst)
			res, err := run.sed(ctx, src, scriptArg.String(), dst)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileSed: %w", err)))
			}
			return transformResultToJS(vm, res)
		})
	}

	// fileWrite(path, data, contentType?) → FileInfo
	if authedFile || publicWriteOK {
		vm.Set("fileWrite", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileWrite: %w", err)))
			}
			dataVal := call.Argument(1)
			if dataVal == nil || goja.IsUndefined(dataVal) || goja.IsNull(dataVal) {
				panic(vm.NewGoError(fmt.Errorf("fileWrite: data is required")))
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
				panic(vm.NewGoError(fmt.Errorf("fileWrite: %w", err)))
			}
			info, err := agent.WriteFile(ctx, path, strings.NewReader(string(data)), contentType)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileWrite: %w", err)))
			}
			// Drop any cached copy under the path actually written (scoped
			// dirs rewrite the key) so a later read re-fetches fresh content.
			run.invalidateCache(string(info.Path))
			return fileInfoToJS(vm, info)
		})

		// fileDelete(path) — folds into Write cap (write on the parent governs unlink).
		vm.Set("fileDelete", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDelete: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpWrite); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDelete: %w", err)))
			}
			if err := agent.DeleteFile(ctx, path); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileDelete: %w", err)))
			}
			run.invalidateCache(path)
			return goja.Undefined()
		})
	}

	// fileList(path, opts?) → FileInfo[] — non-recursive by default.
	if authedFile || publicListOK {
		vm.Set("fileList", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileList: %w", err)))
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
				panic(vm.NewGoError(fmt.Errorf("fileList: %w", err)))
			}
			files, err := agent.ListDir(ctx, path, opts)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileList: %w", err)))
			}
			out := make([]goja.Value, len(files))
			for i, f := range files {
				out[i] = fileInfoToJS(vm, f)
			}
			return vm.ToValue(out)
		})
	}

	// fileStat + fileExists + fileShareURL all gate on Read.
	if authedFile || publicReadOK {
		vm.Set("fileStat", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileStat: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileStat: %w", err)))
			}
			info, err := agent.StatFile(ctx, path)
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileStat: %w", err)))
			}
			return fileInfoToJS(vm, info)
		})

		// fileExists(path) → bool — sugar around fileStat.
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

		// fileShareURL(path, opts?) → { url, expiresAtMs }
		// Returns a presigned, unauthenticated, time-limited URL for the
		// stored file. opts: { expiresInMinutes? } — server defaults to 1h
		// and caps at 24h. For embedding files in markdown links or sharing
		// outside the agent's auth boundary. To show a file in chat, prefer
		// output({type:"file", source:path}) instead.
		vm.Set("fileShareURL", func(call goja.FunctionCall) goja.Value {
			path, err := pathArg(call.Argument(0))
			if err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileShareURL: %w", err)))
			}
			ctx := run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, path, OpRead); err != nil {
				panic(vm.NewGoError(fmt.Errorf("fileShareURL: %w", err)))
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
				panic(vm.NewGoError(fmt.Errorf("fileShareURL: %w", err)))
			}
			out := vm.NewObject()
			out.Set("url", resp.URL)
			out.Set("expiresAtMs", resp.ExpiresAtMs)
			return out
		})
	}

	// output(parts) — share media with the user / channel / calling
	// sibling. Accepts a single DisplayPart object or an array. Type
	// must be image/file/audio/video; prose goes in the LLM's normal
	// reply. Captions live on the media part's `text` field.
	vm.Set("output", func(call goja.FunctionCall) goja.Value {
		parts := parseDisplayParts(vm, call.Argument(0))
		for i, p := range parts {
			if p.Type == "text" {
				panic(vm.NewGoError(fmt.Errorf("output: part %d is type=\"text\"; output is media-only — put prose in your normal reply, or set the `text` field on an image/file/audio/video part to use it as a caption", i)))
			}
			if p.Type != "image" && p.Type != "file" && p.Type != "audio" && p.Type != "video" {
				panic(vm.NewGoError(fmt.Errorf("output: part %d has unsupported type %q; expected image, file, audio, or video", i, p.Type)))
			}
		}
		if err := run.output(run.ctx, parts, ""); err != nil {
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
				if v := opts.Get("allHeaders"); v != nil && !goja.IsUndefined(v) {
					// allHeaders: return every upstream header (default is a
					// curated few — the rest is context-burning noise).
					req.AllHeaders = v.ToBoolean()
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
				// the LLM can pass it directly to fileRead/fileReadBytes/
				// attachToContext/etc.
				result.Set("savedTo", resp.SavedTo)
			}
			if resp.BodyPreview != "" {
				// Head of a saved body so the result is legible without an
				// immediate fileRead(savedTo).
				result.Set("bodyPreview", resp.BodyPreview)
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
					"attachToContext: %s files are not supported by the current model. Supported types: %s. Use fileRead(path) for text-based files",
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

	// Sibling agent (A2A) bindings: agent_<slug>.toolName(args). Each
	// sibling's tool schemas were synced into promptData.Siblings by
	// airlock; the per-user visibility filter (run.visibleSiblings)
	// mirrors what the prompt-render path uses, so the LLM only gets
	// JS bindings for siblings the prompt actually advertised.
	//
	// File arguments and returns are translated at the airlock MCP
	// boundary (cross-bucket S3 copies) — the JS caller just sees path
	// strings the same way same-bucket tools do.
	if len(run.visibleSiblings) > 0 {
		visible := make(map[uuid.UUID]struct{}, len(run.visibleSiblings))
		for _, id := range run.visibleSiblings {
			visible[id] = struct{}{}
		}
		run.agent.syncMu.RLock()
		siblings := run.agent.promptData.Siblings
		run.agent.syncMu.RUnlock()
		for _, s := range siblings {
			if _, ok := visible[s.ID]; !ok {
				continue
			}
			handle := &SiblingHandle{slug: s.Slug, agentID: s.ID, agent: run.agent}
			obj := vm.NewObject()
			siblingSlug := s.Slug
			names := make([]string, len(s.Tools))
			for i, t := range s.Tools {
				names[i] = t.Name
			}
			jsNames := tsrender.JSToolNames(names)
			for _, t := range s.Tools {
				toolName := t.Name
				obj.Set(jsNames[toolName], func(call goja.FunctionCall) goja.Value {
					return invokeSiblingTool(vm, run.ctx, handle, run.id, siblingSlug, toolName, call.Argument(0))
				})
			}
			// NB: open-ended delegation is the top-level `promptAgent`
			// tool (tools.go), not a run_js binding — a suspendable
			// LLM-loop round-trip has no business inside the JS sandbox
			// and must be a first-class pending tool call so Sol's
			// suspend/resume handles it natively. Only the sibling's
			// typed tools stay as run_js bindings here.
			vm.Set("agent_"+siblingSlug, obj)
		}
	}

	return vm
}

// invokeSiblingTool runs an A2A tool call from a JS binding and
// translates the result back to a JS value. Wraps SiblingHandle.CallTool
// with goja error / panic semantics so JS code can try/catch normally.
func invokeSiblingTool(vm *goja.Runtime, ctx context.Context, handle *SiblingHandle, callerRunID, siblingSlug, toolName string, argsArg goja.Value) goja.Value {
	var args any
	if argsArg != nil && !goja.IsUndefined(argsArg) && !goja.IsNull(argsArg) {
		args = argsArg.Export()
	}
	result, err := handle.CallTool(ctx, callerRunID, toolName, args)
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("agent_%s.%s: %w", siblingSlug, toolName, err)))
	}
	return vm.ToValue(result)
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
	// Collect text content blocks. Non-text shapes (resource_link, image,
	// audio) get a best-effort surfacing so a third-party MCP server
	// that returns them doesn't appear silent to the LLM. The A2A path
	// never emits these — airlock's materializer rewrites file paths
	// in-place inside the text block and skips resource_link for agent
	// callers — but we shouldn't rely on that for non-airlock servers.
	var out strings.Builder
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			out.WriteString(c.Text)
		case "resource_link":
			out.WriteString(c.URI)
		case "image":
			out.WriteString("[image ")
			out.WriteString(c.MimeType)
			out.WriteString("]")
		case "audio":
			out.WriteString("[audio ")
			out.WriteString(c.MimeType)
			out.WriteString("]")
		}
	}
	raw := out.String()
	capJSBytes(vm, "mcp_"+mcpSlug+"."+toolName, len(raw))
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

// headersFromJSArg coerces an optional `headers` argument from a JS-side
// conn.request*/ call into the map[string]string the proxy expects. An
// undefined / null / missing arg returns nil — the SDK and proxy both
// treat that as "no per-call overrides". Non-string values are
// stringified via goja's default coercion so the caller doesn't have to
// remember to quote everything.
func headersFromJSArg(v goja.Value) map[string]string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	raw, ok := v.Export().(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if val == nil {
			out[k] = ""
			continue
		}
		out[k] = fmt.Sprint(val)
	}
	return out
}

// connectionErrorForJS rewrites a ConnectionHandle.Request error into the
// shape we want surfaced as a JS throw. AuthRequiredError gets a stable,
// LLM-actionable message (no OAuth URL — that belongs in the UI, not the
// LLM's reply); everything else passes through. Going through NewGoError at
// the call site means goja's *Exception.Unwrap returns this error directly,
// so jsErrorMessage renders its message instead of `[object Object]`.
func connectionErrorForJS(slug string, err error) error {
	if ae, ok := IsAuthRequired(err); ok {
		name := ae.ConnName
		if name == "" {
			name = slug
		}
		return fmt.Errorf("connection %q needs authorization — ask the user to open the agent's Connections tab and set it up", name)
	}
	return err
}

// jsRunError carries the agent/UI-facing message for a run_js failure while
// keeping the original goja error reachable via Unwrap, so errors.Is checks
// (context.Canceled, errJSCPU) still work downstream.
type jsRunError struct {
	msg   string
	cause error
}

func (e *jsRunError) Error() string { return e.msg }
func (e *jsRunError) Unwrap() error { return e.cause }

// jsErrorMessage renders a run_js failure without goja's internal Go stack
// frames (" at github.com/airlockrun/... (native)") or the "GoError:" wrapper
// name, and reports whether the failure originated in Go-native code (a tool,
// proxy, fileRead, or a platform interrupt) versus the agent's own JS.
//
// The native/JS split is structural, not a string heuristic: goja's
// *Exception.Unwrap() returns a non-nil Go error exactly when the thrown
// value is a GoError minted by vm.NewGoError, and *InterruptedError is always
// a platform-side abort. A plain JS throw, a stack overflow, or a syntax
// error carries no Go cause and is the script's own fault.
//
// goja appends its Go call stack to *Exception.Error() and
// *InterruptedError.Error() unconditionally, so going through the typed
// value is the only way to drop it.
func jsErrorMessage(err error) (msg string, native bool) {
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		if u := ie.Unwrap(); u != nil { // CPU limit / ctx cancel reason
			return u.Error(), true
		}
		return "run_js interrupted", true
	}
	var soe *goja.StackOverflowError
	if errors.As(err, &soe) {
		return "Maximum call stack size exceeded", false
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		if u := ex.Unwrap(); u != nil { // GoError wrapping a Go error
			return u.Error(), true
		}
		if v := ex.Value(); v != nil { // plain JS throw
			return renderJSThrownValue(nil, v), false
		}
	}
	return err.Error(), false // compiler/syntax error and the like
}

// renderJSThrownValue turns a JS-thrown value into a string useful to the
// LLM. The hazard is plain `throw {something}` — the default `.String()` on
// a JS object is "[object Object]", which is worse than useless. We prefer
// (in order): an Error's stringification, a `.message` property, a
// JSON.stringify of the object, and only as a last resort `.String()`.
// vm may be nil — JSON.stringify fallback is skipped in that case (an
// *Exception doesn't carry its runtime, so callers off the hot path pass
// nil; the binding-site panics that we control all go through NewGoError
// and never reach this branch).
func renderJSThrownValue(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "undefined"
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		return v.String()
	}
	// Errors and any other value whose ClassName is "Error" stringify
	// cleanly via the default toString (e.g. "TypeError: ...").
	if obj.ClassName() == "Error" {
		return v.String()
	}
	// Honour a .message property if it's a non-empty string — that's the
	// convention for "throw {message, ...}" style.
	if msg := obj.Get("message"); msg != nil && !goja.IsUndefined(msg) && !goja.IsNull(msg) {
		if s := msg.String(); s != "" && s != "[object Object]" {
			return s
		}
	}
	// Otherwise dump the whole object as JSON so the LLM can read the
	// fields. This is also what `console.log({...})` would have shown.
	if vm != nil {
		if stringify, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify")); ok {
			if result, err := stringify(goja.Undefined(), obj); err == nil && result != nil && !goja.IsUndefined(result) && !goja.IsNull(result) {
				if s := result.String(); s != "" && s != "undefined" {
					return s
				}
			}
		}
	}
	// Final fallback. Defensive: never leak "[object Object]" — at least
	// say what the value's class was.
	s := v.String()
	if s == "[object Object]" {
		return "<unserializable " + obj.ClassName() + " thrown>"
	}
	return s
}
