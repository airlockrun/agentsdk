package agentsdk

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
)

const maxManifestBytes = 4 << 20

// Serve emits one canonical declaration manifest to stdout and returns when
// AIRLOCK_AGENT_MODE=manifest. Otherwise it starts the agent runtime and HTTP
// server, blocking until SIGINT/SIGTERM.
func (a *Agent) Serve() {
	switch mode := os.Getenv("AIRLOCK_AGENT_MODE"); mode {
	case "manifest":
		a.serveManifest(os.Stdout)
		return
	case "":
	default:
		panic("agentsdk: unsupported AIRLOCK_AGENT_MODE: " + mode)
	}

	addr := os.Getenv("AIRLOCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := a.Start(ctx); err != nil {
		panic(err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			agentLogger().Warn("close database", zap.Error(err))
		}
	}()

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

func (a *Agent) serveManifest(w io.Writer) {
	payload, err := json.Marshal(a.Manifest())
	if err != nil {
		panic("agentsdk: encode manifest: " + err.Error())
	}
	if len(payload) > maxManifestBytes {
		panic(fmt.Sprintf("agentsdk: manifest exceeds %d bytes", maxManifestBytes))
	}
	payload = append(payload, '\n')
	n, err := w.Write(payload)
	if err != nil {
		panic("agentsdk: write manifest: " + err.Error())
	}
	if n != len(payload) {
		panic("agentsdk: write manifest: short write")
	}
}

// Handler builds the agent's authenticated HTTP mux: capability invocation,
// /webhook, /job, /refresh, /health, tool and asset endpoints plus every
// route registered via RegisterRoute. Custom routes have run and logging
// middleware. Serve installs it after syncing with Airlock. Every request except
// GET/HEAD /health requires exactly one Authorization: Bearer <app token> header,
// including public routes and assets. Airlock authenticates the external caller
// and supplies delivery credentials and attribution to this private listener.
//
// Handler requires a started runtime, validates and freezes registrations, and
// does not listen. Tests use it after agenttest.New to exercise routes through
// the real dispatch (including {param} extraction) with httptest.
func (a *Agent) Handler() http.Handler {
	a.requireRuntime("Handler")
	if strings.TrimSpace(a.token) == "" {
		panic("agentsdk: Handler requires AIRLOCK_AGENT_TOKEN")
	}
	a.freeze()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.RuntimeInvokePath, a.handleRuntimeInvoke)
	mux.HandleFunc("POST /webhook/{name}", a.handleWebhook)
	mux.HandleFunc("POST /job/{name}/{version}", a.handleJob)
	mux.HandleFunc("POST /refresh", a.handleRefresh)
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

	return a.authenticateHost(mux)
}

// authenticateHost verifies possession of the shared app credential before any
// dispatch or attribution parsing. Native app code also holds this credential;
// it is not a sandbox boundary or proof of a human identity on the host.
func (a *Agent) authenticateHost(next http.Handler) http.Handler {
	expected := []byte("Bearer " + a.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/health" {
			// Call only the built-in probe, never a custom mux match or redirect.
			a.handleHealth(w, r)
			return
		}
		var authorization string
		var count int
		for name, values := range r.Header {
			if strings.EqualFold(name, "Authorization") {
				count += len(values)
				if len(values) != 0 {
					authorization = values[0]
				}
			}
		}
		if count != 1 || subtle.ConstantTimeCompare([]byte(authorization), expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		for _, name := range []string{"X-Run-ID", "X-Bridge-ID", jobLeaseTokenHeader, wire.InvocationTokenHeader} {
			count := 0
			for key, values := range r.Header {
				if strings.EqualFold(key, name) {
					count += len(values)
				}
			}
			if count > 1 {
				http.Error(w, "ambiguous delivery headers", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Agent) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	wh, ok := a.webhooks[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	metadata, err := wire.DecodeCallerHeader(r.Header)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	run.invocationToken = r.Header.Get(wire.InvocationTokenHeader)
	run.setCaller(callerFromWire(metadata))
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

// wrapRoute converts a RouteHandlerFunc into http.HandlerFunc and completes the
// Airlock-created route run from the handler's error, panic, and response status.
// Direct Handler tests without an ingress run retain lazy-run behavior.
func (a *Agent) wrapRoute(key string, handler RouteHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metadata, err := wire.DecodeCallerHeader(r.Header)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		caller := callerFromWire(metadata)
		user, _ := caller.User()
		var activeRun *run
		var lazy *lazyRun
		var ctx context.Context
		if runID := r.Header.Get("X-Run-ID"); runID != "" {
			activeRun = newRun(a, runID, "", "", r.Context())
			activeRun.invocationToken = r.Header.Get(wire.InvocationTokenHeader)
			activeRun.setCaller(caller)
			ctx = activeRun.checkedCtx()
		} else {
			lazy = &lazyRun{
				agent:        a,
				triggerRef:   "route:" + key,
				userID:       user.ID,
				callerAccess: caller.Access(),
				caller:       caller,
			}
			ctx = contextWithLazyRun(r.Context(), lazy)
		}
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
			if activeRun != nil {
				completeHTTPRun(ctx, activeRun, sw.status, dispatchErr, panicTrace)
			} else {
				completeLazyRun(ctx, lazy, sw.status, dispatchErr, panicTrace)
			}
		}()

		dispatchErr = handler(sw, r)
	}
}

func completeLazyRun(ctx context.Context, lazy *lazyRun, status int, dispatchErr error, panicTrace string) {
	run := lazy.materialized()
	if run == nil {
		return
	}
	completeHTTPRun(ctx, run, status, dispatchErr, panicTrace)
}

func completeHTTPRun(ctx context.Context, run *run, status int, dispatchErr error, panicTrace string) {
	var err error
	if dispatchErr != nil {
		err = run.complete(ctx, "error", dispatchErr.Error(), wire.ErrorKindAgent, panicTrace)
	} else if status >= http.StatusInternalServerError {
		errMsg := fmt.Sprintf("HTTP status %d", status)
		err = run.complete(ctx, "error", errMsg, wire.ErrorKindAgent, "")
	} else {
		err = run.complete(ctx, "success", "", "", "")
	}
	if err != nil {
		agentLogger().Error("record HTTP run completion failed", zap.String("run_id", run.id), zap.Error(err))
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

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
	webhooks := make([]string, 0, len(a.webhooks))
	for path := range a.webhooks {
		webhooks = append(webhooks, path)
	}
	sort.Strings(webhooks)

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
		Status   string   `json:"status"`
		Webhooks []string `json:"webhooks"`
		Tools    []string `json:"tools"`
	}{status, webhooks, tools}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}
