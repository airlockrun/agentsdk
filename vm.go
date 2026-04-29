package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/websearch"
	"github.com/dop251/goja"
)

// storageKeyArg parses a storage_<slug>.{get,put,...} key argument as
// either a relative-key string or a {zone, key} StorageRef object. Refs
// whose zone doesn't match ownSlug are rejected with a hint pointing at
// the right binding.
func storageKeyArg(vm *goja.Runtime, val goja.Value, ownSlug string) (string, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", fmt.Errorf("key is required")
	}
	if obj, ok := val.Export().(map[string]any); ok {
		zoneAny, hasZone := obj["zone"]
		keyAny, hasKey := obj["key"]
		if !hasZone || !hasKey {
			return "", fmt.Errorf("ref object must have {zone, key}")
		}
		zone, _ := zoneAny.(string)
		key, _ := keyAny.(string)
		if zone == "" || key == "" {
			return "", fmt.Errorf("ref object must have non-empty zone and key")
		}
		if zone != ownSlug {
			return "", fmt.Errorf("ref points at zone %q — call storage_%s instead", zone, zone)
		}
		return key, nil
	}
	return val.String(), nil
}

// listZoneSlugs returns a comma-separated list of registered zone slugs
// the caller can see (read or write), wrapped for use in error messages.
// Filters out AccessInternal zones and ones the caller has no access to,
// so the message doesn't leak hidden zones.
func listZoneSlugs(agent *Agent) string {
	if len(agent.storages) == 0 {
		return "(none)"
	}
	slugs := make([]string, 0, len(agent.storages))
	for slug := range agent.storages {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return strings.Join(slugs, ", ")
}

// mediaResultToJS shapes a *mediaResult as the JS-facing
// { file: {zone, key}, mimeType, size } structure. The composed Key on
// mediaResult is split back into zone + relative key so the LLM gets a
// proper StorageRef object that pairs with storage_<zone>.get(file.key)
// and attachToContext(file).
func mediaResultToJS(vm *goja.Runtime, res *mediaResult) goja.Value {
	zone, key, _ := strings.Cut(res.Key, "/")
	ref := vm.NewObject()
	ref.Set("zone", zone)
	ref.Set("key", key)
	out := vm.NewObject()
	out.Set("file", ref)
	out.Set("mimeType", res.MimeType)
	out.Set("size", res.Size)
	return out
}

// storageRefToComposed normalizes a JS value (relative-key string or
// {zone, key} ref) to the composed "zone/key" form that findStorageByKey
// understands. Used by attachToContext, printToUser source, and other
// surfaces that route via zone slug rather than a bound handle.
func storageRefToComposed(vm *goja.Runtime, val goja.Value) (string, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", fmt.Errorf("ref is required")
	}
	if obj, ok := val.Export().(map[string]any); ok {
		zoneAny, hasZone := obj["zone"]
		keyAny, hasKey := obj["key"]
		if !hasZone || !hasKey {
			return "", fmt.Errorf("ref object must have {zone, key}")
		}
		zone, _ := zoneAny.(string)
		key, _ := keyAny.(string)
		if zone == "" || key == "" {
			return "", fmt.Errorf("ref object must have non-empty zone and key")
		}
		return zone + "/" + key, nil
	}
	s := val.String()
	if s == "" {
		return "", fmt.Errorf("ref is required")
	}
	return s, nil
}

// storageRefArg parses a {zone, key} StorageRef object — used for the
// destination of cross-zone copyTo. Plain strings are not accepted here:
// a destination must always carry an explicit zone.
func storageRefArg(vm *goja.Runtime, val goja.Value) (zone, key string, err error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return "", "", fmt.Errorf("ref is required")
	}
	obj, ok := val.Export().(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("ref must be an object {zone, key}")
	}
	zoneAny, hasZone := obj["zone"]
	keyAny, hasKey := obj["key"]
	if !hasZone || !hasKey {
		return "", "", fmt.Errorf("ref object must have {zone, key}")
	}
	zone, _ = zoneAny.(string)
	key, _ = keyAny.(string)
	if zone == "" || key == "" {
		return "", "", fmt.Errorf("ref object must have non-empty zone and key")
	}
	return zone, key, nil
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

	// Register storage_{slug} objects for each registered Storage zone.
	// Read and Write are gated independently — Get/Stat/List bind only when
	// the caller satisfies Read; Put/Delete/Copy only when the caller
	// satisfies Write. AccessInternal on either axis blocks that axis from
	// JS entirely. Zones where the caller can do nothing aren't bound at all.
	//
	// Methods that take a key accept either a relative-key string or a
	// {zone, key} StorageRef object. Refs whose zone doesn't match the
	// binding's slug error with a clear hint pointing at the right
	// storage_<other> object — so a ref handed back from a tool result
	// can be passed through directly without prefix-stripping.
	for slug, zone := range agent.storages {
		canRead := zone.Read != AccessInternal && accessSatisfies(run.callerAccess, zone.Read)
		canWrite := zone.Write != AccessInternal && accessSatisfies(run.callerAccess, zone.Write)
		if !canRead && !canWrite {
			continue
		}
		handle := &StorageHandle{slug: slug, read: zone.Read, write: zone.Write, agent: agent}
		obj := vm.NewObject()

		if canWrite {
			obj.Set("put", func(call goja.FunctionCall) goja.Value {
				key, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.put: %w", handle.slug, err)))
				}
				data := call.Argument(1).String()
				contentType := call.Argument(2).String()
				if err := handle.Put(run.ctx, key, strings.NewReader(data), contentType); err != nil {
					panic(vm.NewGoError(err))
				}
				return goja.Undefined()
			})

			obj.Set("delete", func(call goja.FunctionCall) goja.Value {
				key, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.delete: %w", handle.slug, err)))
				}
				if err := handle.Delete(run.ctx, key); err != nil {
					panic(vm.NewGoError(err))
				}
				return goja.Undefined()
			})

			obj.Set("copy", func(call goja.FunctionCall) goja.Value {
				src, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copy: %w", handle.slug, err)))
				}
				dst, err := storageKeyArg(vm, call.Argument(1), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copy: %w", handle.slug, err)))
				}
				if err := handle.Copy(run.ctx, src, dst); err != nil {
					panic(vm.NewGoError(err))
				}
				return goja.Undefined()
			})

			// copyTo(src, dstRef) — cross-zone server-side copy. dstRef must
			// be a {zone, key} object referencing a registered zone the
			// caller has Write access to.
			obj.Set("copyTo", func(call goja.FunctionCall) goja.Value {
				src, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copyTo: %w", handle.slug, err)))
				}
				dstZoneSlug, dstKey, err := storageRefArg(vm, call.Argument(1))
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copyTo: dstRef: %w", handle.slug, err)))
				}
				dstZoneCfg, ok := agent.storages[dstZoneSlug]
				if !ok {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copyTo: unknown destination zone %q", handle.slug, dstZoneSlug)))
				}
				if dstZoneCfg.Write == AccessInternal || !accessSatisfies(run.callerAccess, dstZoneCfg.Write) {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.copyTo: caller has no write access to zone %q", handle.slug, dstZoneSlug)))
				}
				dstHandle := &StorageHandle{slug: dstZoneSlug, read: dstZoneCfg.Read, write: dstZoneCfg.Write, agent: agent}
				if err := handle.CopyTo(run.ctx, src, dstHandle, dstKey); err != nil {
					panic(vm.NewGoError(err))
				}
				return goja.Undefined()
			})
		}

		if canRead {
			obj.Set("get", func(call goja.FunctionCall) goja.Value {
				key, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.get: %w", handle.slug, err)))
				}
				rc, err := handle.Get(run.ctx, key)
				if err != nil {
					panic(vm.NewGoError(err))
				}
				defer rc.Close()
				b, err := io.ReadAll(rc)
				if err != nil {
					panic(vm.NewGoError(err))
				}
				return vm.ToValue(string(b))
			})

			obj.Set("stat", func(call goja.FunctionCall) goja.Value {
				key, err := storageKeyArg(vm, call.Argument(0), handle.slug)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("storage_%s.stat: %w", handle.slug, err)))
				}
				info, err := handle.Stat(run.ctx, key)
				if err != nil {
					panic(vm.NewGoError(err))
				}
				res := vm.NewObject()
				res.Set("key", info.Key)
				res.Set("size", info.Size)
				res.Set("contentType", info.ContentType)
				res.Set("lastModified", info.LastModified.Format("2006-01-02T15:04:05Z"))
				return res
			})

			obj.Set("list", func(call goja.FunctionCall) goja.Value {
				prefix := ""
				if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) {
					var err error
					prefix, err = storageKeyArg(vm, call.Argument(0), handle.slug)
					if err != nil {
						panic(vm.NewGoError(fmt.Errorf("storage_%s.list: %w", handle.slug, err)))
					}
				}
				files, err := handle.List(run.ctx, prefix)
				if err != nil {
					panic(vm.NewGoError(err))
				}
				return vm.ToValue(files)
			})
		}

		vm.Set("storage_"+slug, obj)
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
			msg := formatJSValue(vm, call.Argument(0))
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
				// saveAs accepts a {zone, key} StorageRef (preferred) or a
				// composed "zone/key" string. Validate that the zone is
				// registered and the caller has Write access — Airlock just
				// writes wherever we tell it; the gate lives here.
				composed, err := storageRefToComposed(vm, v)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("httpRequest: saveAs: %w", err)))
				}
				zoneSlug, key, ok := strings.Cut(composed, "/")
				if !ok || zoneSlug == "" || key == "" {
					panic(vm.NewGoError(fmt.Errorf(
						"httpRequest: saveAs: missing zone — pass {zone: \"tmp\", key: %q} or \"tmp/%s\"",
						composed, composed)))
				}
				zoneCfg, ok := agent.storages[zoneSlug]
				if !ok {
					panic(vm.NewGoError(fmt.Errorf("httpRequest: saveAs: unknown zone %q — registered zones are %s", zoneSlug, listZoneSlugs(agent))))
				}
				if zoneCfg.Write == AccessInternal || !accessSatisfies(run.callerAccess, zoneCfg.Write) {
					panic(vm.NewGoError(fmt.Errorf("httpRequest: saveAs: caller has no write access to zone %q", zoneSlug)))
				}
				req.SaveAs = composed
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
			// Airlock returns the composed "zone/key" form; expose it as a
			// {zone, key} StorageRef so the LLM can pass it directly to
			// storage_<zone>.get(...) or attachToContext(...).
			zoneSlug, key, _ := strings.Cut(resp.SavedTo, "/")
			ref := vm.NewObject()
			ref.Set("zone", zoneSlug)
			ref.Set("key", key)
			result.Set("savedTo", ref)
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

	// attachToContext(ref) — load an S3 file so the model can see it on the
	// next turn. Accepts either a {zone, key} StorageRef object (preferred,
	// matches what tool returns hand back) or a composed "zone/key" string.
	// Idempotent per run; bytes are collected on run.pendingAttachments and
	// drained into the run_js tool.Result by buildRunJSTool.
	vm.Set("attachToContext", func(call goja.FunctionCall) goja.Value {
		key, err := storageRefToComposed(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("attachToContext: %w", err)))
		}

		// Idempotent: skip if already attached this run.
		run.mu.Lock()
		if run.attachedKeys == nil {
			run.attachedKeys = make(map[string]struct{})
		}
		if _, ok := run.attachedKeys[key]; ok {
			run.mu.Unlock()
			return vm.ToValue("Already in context for this turn.")
		}
		run.attachedKeys[key] = struct{}{}
		run.mu.Unlock()

		zone, relKey, ok := agent.findStorageByKey(key)
		if !ok {
			panic(vm.NewGoError(fmt.Errorf("attachToContext: %q must be a {storage}/{key} path under a registered zone", key)))
		}
		info, err := zone.Stat(run.ctx, relKey)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("attachToContext: file not found: %w", err)))
		}

		if len(run.supportedModalities) > 0 && !mimeMatchesModalities(info.ContentType, run.supportedModalities) {
			panic(vm.NewGoError(fmt.Errorf(
				"attachToContext: %s files are not supported by the current model. Supported types: %s. Use storage_{slug}.get(...) for text-based files",
				info.ContentType, strings.Join(run.supportedModalities, ", "))))
		}

		// Emit a s3ref: sentinel rather than loading + base64-encoding here.
		// Airlock's attachref resolver picks this up on the LLM-stream and
		// session-append paths, canonicalizes to llm/agents/<id>/K, and
		// either presigns a URL or inlines base64. Keeps the agent-side
		// call flat-latency even for huge attachments. The sentinel uses the
		// full {zone}/{key} path so the resolver can presign the right S3
		// object — same shape that lands in S3 for any storage zone write.
		run.mu.Lock()
		run.pendingAttachments = append(run.pendingAttachments, tool.Attachment{
			Data:     "s3ref:" + key,
			MimeType: info.ContentType,
			Filename: key,
		})
		run.mu.Unlock()

		return vm.ToValue(fmt.Sprintf("Attached %s (%s). The file is visible on the next turn.", key, info.ContentType))
	})

	// transcribeAudio(key, opts?) → { text, language?, duration? }
	// Loads bytes from agent storage, runs through the system-default STT
	// model, returns the transcript. Capability-routed: no slug needed.
	vm.Set("transcribeAudio", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if key == "" {
			panic(vm.NewGoError(fmt.Errorf("transcribeAudio: key is required")))
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
		res, err := run.transcribeAudio(run.ctx, key, opts)
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

	// generateImage(prompt, opts?) → { file: {zone, key}, mimeType, size }
	// opts: { saveAs?, size?, aspectRatio?, seed? } — saveAs accepts a
	// {zone, key} StorageRef or a "zone/key" string; omitted writes to
	// the framework's tmp zone with an auto-generated key.
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
				composed, err := storageRefToComposed(vm, v)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("generateImage: saveAs: %w", err)))
				}
				saveAs = composed
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

	// speak(text, opts?) → { file: {zone, key}, mimeType, size }
	// opts: { saveAs?, voice?, outputFormat?, speed? } — saveAs accepts
	// a {zone, key} StorageRef or a "zone/key" string.
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
				composed, err := storageRefToComposed(vm, v)
				if err != nil {
					panic(vm.NewGoError(fmt.Errorf("speak: saveAs: %w", err)))
				}
				saveAs = composed
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

		for _, schema := range mcpSchemas[slug] {
			toolName := schema.Name
			obj.Set(toolName, func(call goja.FunctionCall) goja.Value {
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
	if resp.IsError {
		text := "MCP tool error"
		if len(resp.Content) > 0 {
			text = resp.Content[0].Text
		}
		panic(vm.NewGoError(fmt.Errorf("mcp_%s.%s: %s", mcpSlug, toolName, text)))
	}
	var out strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			out.WriteString(c.Text)
		}
	}
	raw := out.String()
	var parsed any
	if jsonErr := json.Unmarshal([]byte(raw), &parsed); jsonErr == nil {
		return vm.ToValue(parsed)
	}
	return vm.ToValue(raw)
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