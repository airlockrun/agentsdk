package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"go.uber.org/zap"
)

// Serve starts the agent HTTP server. Blocks until SIGINT/SIGTERM.
// Listens on AIRLOCK_ADDR env var or :8080.
// Before starting, syncs connections/webhooks/crons with Airlock.
func (a *Agent) Serve() {
	defer func() {
		if err := a.Close(); err != nil {
			agentLogger().Warn("close database", zap.Error(err))
		}
	}()

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
// Handler validates and freezes registrations, but does not sync with Airlock
// or listen. Tests use it to exercise routes through the real dispatch (including
// {param} extraction) with httptest. A test that needs the synced prompt data
// or MCP schemas a handler reads must call syncWithAirlock first.
func (a *Agent) Handler() http.Handler {
	a.freeze()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", handlePrompt(a))
	mux.HandleFunc("POST /webhook/{name}", a.handleWebhook)
	mux.HandleFunc("POST /fire/{slug}", a.handleFire)
	mux.HandleFunc("POST /refresh", a.handleRefresh)
	mux.HandleFunc("GET /health", a.handleHealth)
	// A2A: airlock's MCP server forwards user-registered tool calls
	// here so sibling agents can invoke them directly (no LLM loop).
	mux.HandleFunc("POST /__air/tool/{name}", a.handleDirectTool)
	// Bundled frontend assets are same-origin so layouts do not depend on a CDN.
	mux.HandleFunc("GET /__air/assets/{name}", a.handleAsset)
	mux.HandleFunc("GET /static/{name}", a.handleStaticAsset)

	// Mount custom routes registered via RegisterRoute.
	// Each route gets a lazy-run installed in ctx — a run is only created
	// if the handler actually makes a model call. The wrapper also logs
	// returned errors, panics, and 5xx responses.
	for key, route := range a.routes {
		mux.HandleFunc(key, a.wrapRoute(key, route.Handler))
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
			run.complete(ctx, "error", errMsg, wire.ErrorKindAgent, trace)
			return
		}
	}()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		ew.WriteError(err)
		run.complete(ctx, "error", err.Error(), wire.ErrorKindPlatform, "")
		return
	}

	if err := wh.Handler(ctx, data, ew); err != nil {
		status := "error"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		}
		ew.WriteError(err)
		run.complete(ctx, status, err.Error(), wire.ErrorKindAgent, "")
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
			run.complete(ctx, "error", errMsg, wire.ErrorKindAgent, trace)
			return
		}
	}()

	if err := h.handler(ctx, ew); err != nil {
		status := "error"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timeout"
		}
		ew.WriteError(err)
		run.complete(ctx, status, err.Error(), wire.ErrorKindAgent, "")
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
	rt, ok := a.tools[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	caller := callerFromRequest(r)
	if !accessSatisfies(caller.Access, rt.access) {
		http.Error(w, `{"error":"tool requires higher access"}`, http.StatusForbidden)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}

	// Bind a lazyRun into ctx so anything the tool reaches for —
	// conn_X.Request, agent.Storage, agent.LLM — can
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
		agent:        a,
		triggerRef:   "mcp-tool:" + name,
		parentRunID:  r.Header.Get("X-Parent-Run-ID"),
		userID:       r.Header.Get("X-User-ID"),
		callerAccess: caller.Access,
	}

	timeout := defaultTimeout
	baseCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	ctx := withCaller(contextWithLazyRun(baseCtx, lazy), caller)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	var dispatchErr error
	var panicTrace string

	defer func() {
		if rec := recover(); rec != nil {
			panicTrace = string(debug.Stack())
			dispatchErr = fmt.Errorf("%v", rec)
			agentLogger().Error("tool panic", zap.String("tool", name), zap.Any("recover", rec), zap.String("stack", panicTrace))
			if !sw.wroteHeader {
				http.Error(sw, `{"error":"tool panicked"}`, http.StatusInternalServerError)
			}
		}
		completeLazyRun(ctx, lazy, sw.status, dispatchErr, panicTrace)
	}()

	res, err := rt.Execute(ctx, raw, tool.CallOptions{})
	if err != nil {
		dispatchErr = err
		http.Error(sw, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	sw.Header().Set("Content-Type", "application/json")
	_, _ = sw.Write([]byte(res.Output))
}

// wrapRoute converts a RouteHandlerFunc into http.HandlerFunc, installing
// a lazy run and caller on r.Context and completing a materialized run from
// the handler's error, panic, and response status.
func (a *Agent) wrapRoute(key string, handler RouteHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Carry the authenticated user (airlock forwards X-User-ID /
		// X-User-Email on authed proxied requests) so UserFromContext works
		// in route handlers — without materializing a run.
		caller := callerFromRequest(r)
		lazy := &lazyRun{
			agent:           a,
			triggerRef:      "route:" + key,
			userID:          r.Header.Get("X-User-ID"),
			userEmail:       r.Header.Get("X-User-Email"),
			userDisplayName: r.Header.Get("X-User-Name"),
			callerAccess:    caller.Access,
		}
		ctx := withCaller(contextWithLazyRun(r.Context(), lazy), caller)
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		var dispatchErr error
		var panicTrace string

		defer func() {
			if rec := recover(); rec != nil {
				panicTrace = string(debug.Stack())
				dispatchErr = fmt.Errorf("%v", rec)
				agentLogger().Error("route panic", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Any("recover", rec), zap.String("stack", panicTrace))
				if !sw.wroteHeader {
					http.Error(sw, "internal server error", http.StatusInternalServerError)
				}
			}
			if dispatchErr != nil && panicTrace == "" {
				agentLogger().Error("route error", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Error(dispatchErr))
				if !sw.wroteHeader {
					var httpErr *HTTPError
					if errors.As(dispatchErr, &httpErr) {
						http.Error(sw, httpErr.Message, httpErr.Status)
					} else {
						http.Error(sw, "internal server error", http.StatusInternalServerError)
					}
				}
			}
			if sw.status >= http.StatusInternalServerError {
				agentLogger().Warn("route error response", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Int("status", sw.status))
			}
			completeLazyRun(ctx, lazy, sw.status, dispatchErr, panicTrace)
		}()

		dispatchErr = handler(sw, r)
	}
}

func callerFromRequest(r *http.Request) caller {
	access := Access(r.Header.Get("X-Caller-Access"))
	if access == "" {
		access = AccessPublic
	}
	return caller{
		Access: access,
		UserID: r.Header.Get("X-User-ID"),
		RunID:  r.Header.Get("X-Parent-Run-ID"),
	}
}

func completeLazyRun(ctx context.Context, lazy *lazyRun, status int, dispatchErr error, panicTrace string) {
	run := lazy.materialized()
	if run == nil {
		return
	}
	if dispatchErr != nil {
		_ = run.complete(ctx, "error", dispatchErr.Error(), wire.ErrorKindAgent, panicTrace)
		return
	}
	if status >= http.StatusInternalServerError {
		errMsg := fmt.Sprintf("HTTP status %d", status)
		_ = run.complete(ctx, "error", errMsg, wire.ErrorKindAgent, "")
		return
	}
	_ = run.complete(ctx, "success", "", "", "")
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.wroteHeader {
		return
	}
	sw.status = code
	sw.wroteHeader = true
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

	// A healthy report must mean the DB is reachable AND the credentials
	// authenticate. The dispatcher gates traffic on this endpoint; without
	// the check a 200 here lets it route to an agent that 500s on its first
	// query — e.g. a drifted DB role (pq 28P01) on an agent with no
	// migrations, where autoMigrate never forced a boot-time connection.
	// Reporting 503 instead keeps the agent out of rotation until its creds
	// are reconciled (the builder re-asserts the role on upgrade).
	status := "ok"
	code := http.StatusOK
	pingCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	pingErr := a.DB().PingContext(pingCtx)
	cancel()
	if pingErr != nil {
		status = "db_unavailable"
		code = http.StatusServiceUnavailable
		agentLogger().Warn("health: db ping failed", zap.Error(pingErr))
	}

	resp := struct {
		Status    string         `json:"status"`
		Webhooks  []string       `json:"webhooks"`
		Schedules []scheduleInfo `json:"schedules"`
		Tools     []string       `json:"tools"`
	}{status, webhooks, schedules, tools}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}
