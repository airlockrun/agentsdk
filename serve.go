package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"
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

	// Start conversation VM garbage collection.
	a.startConvVMGC(a.convVMConfig)

	// Start the background-run flusher. Closes any stale ambient run after
	// the inactivity window elapses.
	a.startBackgroundFlusher()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", handlePrompt(a))
	mux.HandleFunc("POST /webhook/{name}", a.handleWebhook)
	mux.HandleFunc("POST /cron/{name}", a.handleCron)
	mux.HandleFunc("POST /refresh", a.handleRefresh)
	mux.HandleFunc("GET /health", a.handleHealth)

	// Mount custom routes registered via RegisterRoute.
	// Each route gets a lazy-run installed in ctx — a run is only created
	// if the handler actually makes a model call. Wrap with logging
	// middleware so panics surface in docker logs.
	for key, route := range a.routes {
		mux.HandleFunc(key, routeLogging(a.wrapRoute(key, route.Handler)))
	}

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		// Flush any open background run before the process exits.
		a.stopBackgroundFlusher()
	}()

	log.Printf("agentsdk: version=%s serving on %s", Version, addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic("agentsdk: server error: " + err.Error())
	}
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

func (a *Agent) handleCron(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cr, ok := a.crons[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	timeout := cr.Timeout
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
	run.callerAccess = AccessAdmin // cron is a trusted scheduled trigger
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

	if err := cr.Handler(ctx, ew); err != nil {
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

// wrapRoute converts a RouteHandlerFunc into http.HandlerFunc, installing
// a lazy run into ctx and flushing the run on return if it was materialized.
func (a *Agent) wrapRoute(key string, handler RouteHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lazy := &lazyRun{agent: a, triggerRef: "route:" + key}
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
				log.Printf("agentsdk: route panic: %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		if sw.status >= 500 {
			log.Printf("agentsdk: route error: %s %s → %d", r.Method, r.URL.Path, sw.status)
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
		log.Printf("agentsdk: /refresh sync failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	type cronInfo struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
	}

	webhooks := make([]string, 0, len(a.webhooks))
	for path := range a.webhooks {
		webhooks = append(webhooks, path)
	}
	sort.Strings(webhooks)

	crons := make([]cronInfo, 0, len(a.crons))
	for name, cr := range a.crons {
		crons = append(crons, cronInfo{Name: name, Schedule: cr.Schedule})
	}

	tools := make([]string, 0, len(a.tools))
	for name := range a.tools {
		tools = append(tools, name)
	}
	sort.Strings(tools)

	resp := struct {
		Status   string     `json:"status"`
		Webhooks []string   `json:"webhooks"`
		Crons    []cronInfo `json:"crons"`
		Tools    []string   `json:"tools"`
	}{"ok", webhooks, crons, tools}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
