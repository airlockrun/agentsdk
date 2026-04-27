package agentsdk

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	_ "github.com/lib/pq" // register "postgres" driver for agent.DB()
	"go.uber.org/zap"
)

// Config holds configuration for creating an Agent.
type Config struct {
	Description string // required — shown to users in the Airlock UI
}

// Agent is a long-lived singleton, one per container.
// Created once at startup via New(), lives for the lifetime of the process.
type Agent struct {
	agentID     string
	apiURL      string
	token       string
	description string
	httpClient  *http.Client
	client      *airlockClient

	db     *sql.DB
	dbOnce sync.Once

	sensitiveSet map[string]struct{}
	sensitiveM   sync.RWMutex

	tools    map[string]*registeredTool
	webhooks map[string]*Webhook
	crons    map[string]*Cron
	routes   map[string]*Route
	auths    map[string]*Connection
	mcps     map[string]*MCP
	topics   map[string]*Topic
	storages map[string]*Storage

	extraPrompts []*ExtraPrompt   // access-scoped system prompt fragments; see AddExtraPrompt
	modelSlots   []*ModelSlot     // named model slots; see RegisterModel

	// Airlock-owned state: rendered/discovered server-side at sync time and
	// pushed back via SyncResponse. /refresh re-runs sync to pick up changes
	// (e.g. MCP OAuth completion) without restarting the container.
	syncMu            sync.RWMutex
	systemPrompt      string                     // rendered by Airlock
	mcpSchemas        map[string][]MCPToolSchema // server slug → discovered tools
	publicStorageBase string                     // base URL for AccessPublic zone reads (subdomain or host-level fallback)

	// Deps holds application-level dependencies (connection handles, MCP handles, etc.).
	// The builder defines their own typed struct and assigns it here.
	// Access from handlers via GetDeps[T](run).
	Deps any

	conversationVMs sync.Map // map[string]*ConversationVM
	convVMConfig    ConversationVMConfig

	// bg holds the rolling "background" run used for model calls made with
	// no dispatcher-bound ctx. See background.go.
	bg backgroundState

	// logger is used by Agent.Log when no run is bound to ctx.
	logger *zap.Logger
}

// GetDeps retrieves the typed Deps struct from the Agent bound to ctx.
// Panics if no agent is bound (must be called from inside a handler), if
// Deps is nil, or if the type doesn't match. Used by handlers that need
// the builder's pre-registered application state (connection/MCP handles,
// config, etc.) — particularly VM functions defined in separate packages
// where the `agent` variable isn't in scope.
func GetDeps[T any](ctx context.Context) T {
	a := AgentFromContext(ctx)
	if a == nil {
		panic("agentsdk: GetDeps: no agent bound to ctx — call from inside a handler or tool Execute")
	}
	v, ok := a.Deps.(T)
	if !ok {
		panic("agentsdk: GetDeps type mismatch — check that agent.Deps is set to the correct type")
	}
	return v
}

// AgentFromContext returns the *Agent associated with a handler's ctx.
// Returns nil if ctx wasn't produced by a handler (e.g. a plain
// context.Background() in test code).
func AgentFromContext(ctx context.Context) *Agent {
	if r := runFromContext(ctx); r != nil {
		return r.agent
	}
	if l := lazyRunFromContext(ctx); l != nil {
		return l.agent
	}
	return nil
}

// New creates an Agent by reading required environment variables.
// Panics if AIRLOCK_AGENT_ID, AIRLOCK_API_URL, or AIRLOCK_AGENT_TOKEN is missing.
// Panics if Config.Description is empty.
func New(cfg Config) *Agent {
	if cfg.Description == "" {
		panic("agentsdk: Config.Description is required")
	}

	agentID := requireEnv("AIRLOCK_AGENT_ID")
	apiURL := requireEnv("AIRLOCK_API_URL")
	token := requireEnv("AIRLOCK_AGENT_TOKEN")

	a := &Agent{
		agentID:      agentID,
		apiURL:       apiURL,
		token:        token,
		description:  cfg.Description,
		httpClient:   &http.Client{},
		sensitiveSet: make(map[string]struct{}),
		tools:        make(map[string]*registeredTool),
		webhooks: make(map[string]*Webhook),
		crons:    make(map[string]*Cron),
		routes:   make(map[string]*Route),
		auths:    make(map[string]*Connection),
		mcps:     make(map[string]*MCP),
		topics:   make(map[string]*Topic),
		storages: make(map[string]*Storage),
		convVMConfig: DefaultConversationVMConfig(),
	}
	a.client = newAirlockClient(apiURL, token, a.httpClient)
	a.AddSensitive(token)
	a.autoMigrate()
	logger, _ := zap.NewProduction()
	if logger == nil {
		logger = zap.NewNop()
	}
	a.logger = logger
	// Framework-owned scratch zone — used by run_js output truncation and
	// generated media. Builders may pass Slug:"tmp" to RegisterStorage; the
	// register helper silently no-ops in that case so both sides share the
	// same handle.
	a.storages[reservedTmpSlug] = &Storage{
		Slug:        reservedTmpSlug,
		Read:        AccessUser,
		Write:       AccessUser,
		Description: "Ephemeral run output (auto-managed by the framework — truncated tool output, generated media).",
	}
	return a
}

// Log records a message scoped to the current handler invocation at the
// given level. Visible in the Runs UI alongside the actions the handler
// performed; level controls how the UI surfaces it (color/filter).
//
// Use LogLevelInfo for normal progress, LogLevelWarn for recoverable
// concerns, and LogLevelError for failures the handler chose not to
// raise. The argument shape is uniform — pick a level rather than reaching
// for severity-named methods.
func (a *Agent) Log(ctx context.Context, level LogLevel, msg string) {
	if r := a.runForCall(ctx); r != nil {
		r.logAppend(level, msg)
		return
	}
	switch level {
	case LogLevelError:
		a.logger.Error(msg)
	case LogLevelWarn:
		a.logger.Warn(msg)
	default:
		a.logger.Info(msg)
	}
}

// Logf is the printf-style sibling of Log — formats with fmt.Sprintf and
// records the result. Use Log for plain strings, Logf when you'd otherwise
// reach for fmt.Sprintf.
func (a *Agent) Logf(ctx context.Context, level LogLevel, format string, args ...any) {
	a.Log(ctx, level, fmt.Sprintf(format, args...))
}

// DB returns a lazily-initialized *sql.DB from AIRLOCK_DB_URL.
// Returns nil if the env var is not set (DB is optional).
func (a *Agent) DB() *sql.DB {
	a.dbOnce.Do(func() {
		dsn := os.Getenv("AIRLOCK_DB_URL")
		if dsn == "" {
			return
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			panic("agentsdk: failed to open database: " + err.Error())
		}
		a.db = db
	})
	return a.db
}

// --- Conversation attachments ---

// Attachment retrieves a conversation file attachment by its opaque
// fileID. Different surface from the zoned Storage handles — attachments
// are content the user uploaded to a chat message and aren't builder-keyed.
func (a *Agent) Attachment(ctx context.Context, fileID string) (io.ReadCloser, error) {
	resp, err := a.client.do(ctx, "GET", "/api/agent/files/"+fileID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("agentsdk: get attachment %s: status %d", fileID, resp.StatusCode)
	}
	return resp.Body, nil
}

// systemPromptSnapshot returns the cached system prompt last rendered by
// Airlock. Mutex-guarded so concurrent /refresh writes don't race the read.
// Lowercase deliberately — builders never need this; only solagent.go reads
// it when assembling the Sol agent for a run.
func (a *Agent) systemPromptSnapshot() string {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	return a.systemPrompt
}

// snapshotMCPSchemas returns a value-copy of the MCP schema map. Callers
// (e.g. vm.go) work against the snapshot for the duration of a run so a
// concurrent /refresh can't mutate the map mid-iteration.
func (a *Agent) snapshotMCPSchemas() map[string][]MCPToolSchema {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	if a.mcpSchemas == nil {
		return nil
	}
	out := make(map[string][]MCPToolSchema, len(a.mcpSchemas))
	for k, v := range a.mcpSchemas {
		out[k] = v
	}
	return out
}

// applySyncResponse atomically replaces the cached system prompt + MCP
// schemas + public storage base URL with what Airlock returned from a
// sync round-trip. Called both at startup (from syncWithAirlock in
// sync.go) and on /refresh.
func (a *Agent) applySyncResponse(resp SyncResponse) {
	a.syncMu.Lock()
	a.systemPrompt = resp.SystemPrompt
	a.mcpSchemas = resp.MCPSchemas
	a.publicStorageBase = resp.PublicStorageBase
	a.syncMu.Unlock()
}

// publicStorageBaseSnapshot returns the cached public-storage base URL.
// Mutex-guarded so concurrent /refresh writes don't race the read.
func (a *Agent) publicStorageBaseSnapshot() string {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	return a.publicStorageBase
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("agentsdk: required environment variable " + key + " is not set")
	}
	return v
}
