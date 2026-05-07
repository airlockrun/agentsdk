package agentsdk

import (
	"context"
	"database/sql"
	"fmt"
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

	db     *AgentDB
	dbOnce sync.Once

	sensitiveSet map[string]struct{}
	sensitiveM   sync.RWMutex

	tools       map[string]*registeredTool
	webhooks    map[string]*Webhook
	crons       map[string]*Cron
	routes      map[string]*Route
	auths       map[string]*Connection
	mcps        map[string]*MCP
	envVars     map[string]*EnvVar
	topics      map[string]*Topic
	directories []*Directory // registration order; longest-prefix wins at lookup

	extraPrompts []*ExtraPrompt   // access-scoped system prompt fragments; see AddExtraPrompt
	modelSlots   []*ModelSlot     // named model slots; see RegisterModel

	// Airlock-owned state: rendered/discovered server-side at sync time and
	// pushed back via SyncResponse. /refresh re-runs sync to pick up changes
	// (e.g. MCP OAuth completion) without restarting the container.
	syncMu sync.RWMutex
	// systemPrompt is the unfiltered admin variant; systemPromptUser and
	// systemPromptPublic carry the access-filtered variants returned by
	// Airlock at sync time. solagent.go selects per-run via callerAccess
	// with no cross-tier fallback — see systemPromptSnapshot.
	systemPrompt       string
	systemPromptUser   string
	systemPromptPublic string
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
		webhooks:     make(map[string]*Webhook),
		crons:        make(map[string]*Cron),
		routes:       make(map[string]*Route),
		auths:        make(map[string]*Connection),
		mcps:         make(map[string]*MCP),
		envVars:      make(map[string]*EnvVar),
		topics:       make(map[string]*Topic),
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
	// Framework-owned scratch directory — used by run_js output truncation
	// and generated media. Builders may RegisterDirectory("tmp", ...); the
	// register helper preserves the framework's caps (the description may
	// still be supplied) so both sides share the same directory.
	a.directories = append(a.directories, &Directory{
		Path:           reservedTmpPath,
		Read:           AccessUser,
		Write:          AccessUser,
		List:           AccessUser,
		Description:    "Ephemeral scratch (auto-managed by the framework — truncated tool output, generated media).",
		RetentionHours: 72, // sweeper drops files older than 3 days
	})
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

// DB returns a lazily-initialized *AgentDB from AIRLOCK_DB_URL. Returns
// nil if the env var is not set (DB is optional).
//
// AgentDB implements the same DBTX interface that sqlc-generated New()
// takes, so `mygen.New(agent.DB())` works unchanged. The wrapper is the
// extension point through which the framework can later record query
// activity onto the run carried by ctx.
func (a *Agent) DB() *AgentDB {
	a.dbOnce.Do(func() {
		dsn := os.Getenv("AIRLOCK_DB_URL")
		if dsn == "" {
			return
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			panic("agentsdk: failed to open database: " + err.Error())
		}
		a.db = &AgentDB{db: db, agent: a}
	})
	return a.db
}

// systemPromptSnapshot returns the cached system prompt last rendered by
// Airlock for the given caller access. Mutex-guarded so concurrent
// /refresh writes don't race the read. Lowercase deliberately — builders
// never need this; only solagent.go reads it when assembling the Sol
// agent for a run.
//
// One variant per tier — no fallback. If Airlock returned empty for the
// matching tier, that's what the run gets; an empty system prompt is a
// loud, visible failure mode by design (the agent operator notices
// missing capabilities) instead of a silent admin-prompt leak. Empty
// caller is treated as AccessUser to match accessSatisfies's default.
// Unknown access values panic — they can only happen via a wire-shape
// bug, and silently mapping them to "user" would mask it.
func (a *Agent) systemPromptSnapshot(caller Access) string {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	switch caller {
	case AccessAdmin:
		return a.systemPrompt
	case AccessUser, "":
		return a.systemPromptUser
	case AccessPublic:
		return a.systemPromptPublic
	}
	panic("agentsdk: systemPromptSnapshot: unknown caller access " + string(caller))
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
	a.systemPromptUser = resp.SystemPromptUser
	a.systemPromptPublic = resp.SystemPromptPublic
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
