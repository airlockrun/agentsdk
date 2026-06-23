package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// Serve starts the agent HTTP server. Blocks until SIGINT/SIGTERM.
// Listens on AIRLOCK_ADDR env var or :8080.
// Before starting, syncs connections/webhooks/crons with Airlock.
func (a *Agent) Serve() {
	addr := os.Getenv("AIRLOCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Sync with Airlock before accepting requests. syncOrPanic preserves the
	// historical "fail loud at boot" behaviour; the underlying syncWithAirlock
	// is also called from /refresh where errors propagate to Airlock.
	a.syncOrPanic(ctx)

	// Start the background-run flusher. Closes any stale ambient run after
	// the inactivity window elapses.
	a.startBackgroundFlusher()

	server := &http.Server{
		Addr:    addr,
		Handler: a.Handler(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		// Flush any open background run before the process exits.
		a.stopBackgroundFlusher()
	}()

	agentLogger().Info("serving", zap.String("version", Version), zap.String("addr", addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic("agentsdk: server error: " + err.Error())
	}
}

// Handler builds the agent's HTTP mux: the framework routes (/prompt,
// /webhook, /fire, /refresh, /health, the A2A and asset endpoints) plus every
// route registered via RegisterRoute, each wrapped with the lazy-run + logging
// middleware. Serve installs it after syncing with Airlock.
//
// Handler does not sync with Airlock and does not listen — it just returns the
// mux. Tests use it to exercise routes through the real dispatch (including
// {param} extraction) with httptest. A test that needs the synced prompt data
// or MCP schemas a handler reads must call syncWithAirlock first.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", handlePrompt(a))
	mux.HandleFunc("POST /webhook/{name}", a.handleWebhook)
	mux.HandleFunc("POST /fire/{slug}", a.handleFire)
	mux.HandleFunc("POST /refresh", a.handleRefresh)
	mux.HandleFunc("GET /health", a.handleHealth)
	// A2A: airlock's MCP server forwards user-registered tool calls
	// here so sibling agents can invoke them directly (no LLM loop).
	mux.HandleFunc("POST /__air/tool/{name}", a.handleDirectTool)
	// Bundled frontend assets (htmx, pico.css) — same-origin so the
	// scaffold layout doesn't depend on a CDN.
	mux.HandleFunc("GET /__air/assets/{name}", a.handleAsset)

	// Mount custom routes registered via RegisterRoute.
	// Each route gets a lazy-run installed in ctx — a run is only created
	// if the handler actually makes a model call. Wrap with logging
	// middleware so panics surface in docker logs.
	for key, route := range a.routes {
		mux.HandleFunc(key, routeLogging(a.wrapRoute(key, route.Handler)))
	}

	return mux
}

func (a *Agent) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	wh, ok := a.webhooks[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	timeout := wh.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	runID := r.Header.Get("X-Run-ID")
	if runID == "" {
		panic("agentsdk: X-Run-ID header is required")
	}
	bridgeID := r.Header.Get("X-Bridge-ID")

	run := newRun(a, runID, bridgeID, "", ctx)
	run.callerAccess = AccessAdmin // webhook is a trusted server trigger
	ctx = contextWithRun(ctx, run)
	ew := newEventWriter(w)

	defer func() {
		if rec := recover(); rec != nil {
			trace := string(debug.Stack())
			errMsg := fmt.Sprintf("%v", rec)
			ew.WriteError(fmt.Errorf("%s", errMsg))
			run.complete(ctx, "error", errMsg, ErrorKindAgent, trace)
			return
		}
	}()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		ew.WriteError(err)
		run.complete(ctx, "error", err.Error(), ErrorKindPlatform, "")
		return
	}

	if err := wh.Handler(ctx, data, ew); err != nil {
		status := "error"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		}
		ew.WriteError(err)
		run.complete(ctx, status, err.Error(), ErrorKindAgent, "")
		return
	}
	run.complete(ctx, "success", "", "", "")
}

// handleFire serves a scheduler-driven fire of a registered cron or schedule
// handler. The X-Fire-ID header identifies the fire row so a schedule handler
// can look up its per-instance data in the agent's own DB (ScheduleFromContext).
func (a *Agent) handleFire(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	h, ok := a.scheduleHandlers[slug]
	if !ok {
		http.NotFound(w, r)
		return
	}

	timeout := h.timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	runID := r.Header.Get("X-Run-ID")
	if runID == "" {
		panic("agentsdk: X-Run-ID header is required")
	}
	bridgeID := r.Header.Get("X-Bridge-ID")

	run := newRun(a, runID, bridgeID, "", ctx)
	run.callerAccess = AccessAdmin // a timed fire is a trusted scheduled trigger
	run.fireID = r.Header.Get("X-Fire-ID")
	run.fireSlug = slug
	ctx = contextWithRun(ctx, run)
	ew := newEventWriter(w)

	defer func() {
		if rec := recover(); rec != nil {
			trace := string(debug.Stack())
			errMsg := fmt.Sprintf("%v", rec)
			ew.WriteError(fmt.Errorf("%s", errMsg))
			run.complete(ctx, "error", errMsg, ErrorKindAgent, trace)
			return
		}
	}()

	if err := h.handler(ctx, ew); err != nil {
		status := "error"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		}
		ew.WriteError(err)
		run.complete(ctx, status, err.Error(), ErrorKindAgent, "")
		return
	}
	run.complete(ctx, "success", "", "", "")
}

// handleDirectTool dispatches a user-registered tool by name without
// running the LLM loop. Used by Airlock's MCP server endpoint to expose
// tools to sibling agents (A2A): the calling agent sees a typed
// `agent_<slug>.toolName(...)` binding, the MCP server forwards the
// call to airlock, and airlock forwards here with the resolved
// caller access in X-Caller-Access.
//
// Access gating mirrors what the VM does at call time: the caller's
// access must be >= the tool's registered Access (typically AccessUser).
// Reject otherwise with 403 — the MCP server propagates that as a
// JSON-RPC error to the caller.
func (a *Agent) handleDirectTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tool, ok := a.tools[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	caller := Access(r.Header.Get("X-Caller-Access"))
	if caller == "" {
		caller = AccessUser
	}
	if !accessSatisfies(caller, tool.Access) {
		http.Error(w, `{"error":"tool requires higher access"}`, http.StatusForbidden)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}

	// Bind a lazyRun into ctx so anything the tool reaches for —
	// GetDeps[T], conn_X.Request, agent.Storage, agent.LLM — can
	// resolve the Agent (and materialize a real run if it actually
	// performs an LLM call / log / action). Without this, the tool
	// gets a bare http.Request ctx and any AgentFromContext lookup
	// panics. Mirrors the lazyRun setup wrapRoute uses for custom
	// HTTP routes.
	//
	// Scope keys (parentRun/user) ride on headers airlock sets for
	// A2A and external MCP tool calls; CheckFileAccess consults them
	// when gating reads on scoped directories.
	lazy := &lazyRun{
		agent:       a,
		triggerRef:  "mcp-tool:" + name,
		parentRunID: r.Header.Get("X-Parent-Run-ID"),
		userID:      r.Header.Get("X-User-ID"),
	}

	timeout := defaultTimeout
	baseCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	ctx := contextWithLazyRun(baseCtx, lazy)

	defer func() {
		if rec := recover(); rec != nil {
			agentLogger().Error("tool panic", zap.String("tool", name), zap.Any("recover", rec), zap.ByteString("stack", debug.Stack()))
			http.Error(w, `{"error":"tool panicked"}`, http.StatusInternalServerError)
		}
		// If the tool materialized a real run (made an LLM call,
		// wrote actions, etc.) flush its terminal state so airlock
		// records the run as success and the action timeline is
		// persisted. Best-effort — failures here don't change the
		// already-written response.
		if run := lazy.materialized(); run != nil {
			_ = run.complete(ctx, "success", "", "", "")
		}
	}()

	out, err := tool.Execute(ctx, raw)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(out))
}

// wrapRoute converts a RouteHandlerFunc into http.HandlerFunc, installing
// a lazy run into ctx and flushing the run on return if it was materialized.
func (a *Agent) wrapRoute(key string, handler RouteHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Carry the authenticated user (airlock forwards X-User-ID /
		// X-User-Email on authed proxied requests) so UserFromContext works
		// in route handlers — without materializing a run.
		lazy := &lazyRun{
			agent:           a,
			triggerRef:      "route:" + key,
			userID:          r.Header.Get("X-User-ID"),
			userEmail:       r.Header.Get("X-User-Email"),
			userDisplayName: r.Header.Get("X-User-Name"),
		}
		ctx := contextWithLazyRun(r.Context(), lazy)
		defer func() {
			if run := lazy.materialized(); run != nil {
				_ = run.complete(ctx, "success", "", "", "")
			}
		}()
		handler(ctx, w, r)
	}
}

// routeLogging wraps a route handler with panic recovery and error logging.
func routeLogging(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				agentLogger().Error("route panic", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Any("recover", rec), zap.ByteString("stack", debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		if sw.status >= 500 {
			agentLogger().Warn("route error", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Int("status", sw.status))
		}
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// handleRefresh re-runs syncWithAirlock so the cached system prompt and MCP
// schemas pick up server-side changes (typically OAuth completion for an MCP
// server). Synchronous: the response only returns once sync has applied, so
// callers (Airlock dispatcher) know the agent is in the new state on 200.
func (a *Agent) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if err := a.syncWithAirlock(r.Context()); err != nil {
		agentLogger().Error("/refresh sync failed", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	type scheduleInfo struct {
		Slug string `json:"slug"`
		Kind string `json:"kind"`
	}

	webhooks := make([]string, 0, len(a.webhooks))
	for path := range a.webhooks {
		webhooks = append(webhooks, path)
	}
	sort.Strings(webhooks)

	schedules := make([]scheduleInfo, 0, len(a.scheduleHandlers))
	for slug, h := range a.scheduleHandlers {
		schedules = append(schedules, scheduleInfo{Slug: slug, Kind: h.kind})
	}

	tools := make([]string, 0, len(a.tools))
	for name := range a.tools {
		tools = append(tools, name)
	}
	sort.Strings(tools)

	resp := struct {
		Status    string         `json:"status"`
		Webhooks  []string       `json:"webhooks"`
		Schedules []scheduleInfo `json:"schedules"`
		Tools     []string       `json:"tools"`
	}{"ok", webhooks, schedules, tools}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
