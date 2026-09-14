package agentsdk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/airlockrun/agentsdk/internal/binding"

	"github.com/airlockrun/agentsdk/wire"
	_ "github.com/lib/pq" // register "postgres" driver for agent.DB()
	"go.uber.org/zap"
)

// Config holds configuration for creating an Agent.
type Config struct {
	noUnkeyedLiterals

	Description string // required — shown to users in the Airlock UI
	// Emoji is an optional decorative glyph shown next to the agent in
	// the Airlock UI (agent list, sidebar, header). Purely cosmetic;
	// empty means "no emoji". A short grapheme is expected (a single
	// emoji incl. ZWJ / skin-tone / flag sequences) — it is NOT
	// validated to one rune; over-long/garbage values are dropped
	// server-side rather than failing the sync.
	Emoji string
}

type agentPhase uint8

const (
	agentDefining agentPhase = iota
	agentInitializing
	agentStarting
	agentRunning
	agentClosed
)

// Agent is a long-lived singleton, one per container. New creates it in the
// definition phase; Serve starts its runtime after registrations are complete.
type Agent struct {
	phaseM sync.RWMutex
	phase  agentPhase

	agentID     string
	apiURL      string
	token       string
	description string
	emoji       string
	httpClient  *http.Client
	client      *airlockClient

	db *AgentDB

	sensitiveSet  map[string]struct{}
	sensitiveM    sync.RWMutex
	registrationM sync.Mutex
	frozen        bool

	tools            map[string]*registeredTool
	agentDefinitions map[string]*registeredAgent
	webhooks         map[string]*Webhook
	jobs             map[jobKey]*registeredJob
	jobCrons         map[string]*registeredJobCron
	routes           map[string]*Route
	auths            map[string]*Connection
	mcps             map[string]*MCP
	envVars          map[string]*EnvVar
	topics           map[string]*Topic
	staticAssets     map[string]*StaticAsset
	directories      []*directory // registration order; longest-prefix wins at lookup
	connectors       map[string]*Connector

	instructions []*Instruction // access-scoped system prompt fragments; see AddInstruction
	modelSlots   []*ModelSlot   // named model slots; see RegisterModel
	startHooks   []startHook

	// Airlock-owned state: discovered server-side at sync time and pushed
	// back via syncResponse. /refresh re-runs sync to pick up changes
	// (e.g. MCP OAuth completion) without restarting the container.
	syncMu            sync.RWMutex
	promptData        wire.PromptData // platform URLs, filled by applySyncResponse
	publicStorageBase string          // base URL for AccessPublic zone reads (subdomain or host-level fallback)

	// bg holds the rolling "background" run used for model calls made with
	// no dispatcher-bound ctx. See background.go.
	bg backgroundState
}

// ErrAgentURLUnavailable means the agent's public URL is not available from
// the current value or context. Serve syncs with Airlock before accepting
// requests, so deployed handlers normally have a URL; tests that call Handler
// directly may need to sync first or omit absolute links.
var ErrAgentURLUnavailable = errors.New("agentsdk: agent URL is unavailable")

// AgentURL returns the agent's public origin, e.g. https://todo.example.com.
// Use relative paths such as "/auth" for links within the same agent UI; use
// AgentURL only when an absolute URL leaves the current request context, such
// as emails, third-party callbacks, or messages.
func (a *Agent) AgentURL() (string, error) {
	if !a.runtimeAvailable() {
		return "", a.runtimeUnavailable("AgentURL")
	}
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	if a.promptData.AgentRouteURL == "" {
		return "", ErrAgentURLUnavailable
	}
	return a.promptData.AgentRouteURL, nil
}

// AgentURLFromContext returns AgentURL for the agent bound to ctx.
func AgentURLFromContext(ctx context.Context) (string, error) {
	a := AgentFromContext(ctx)
	if a == nil {
		return "", ErrAgentURLUnavailable
	}
	return a.AgentURL()
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

// New creates an Agent for dependency wiring and registrations. It performs no
// database, network, credential, migration, or other runtime initialization;
// Serve starts the runtime after declarations are complete. Panics if
// Config.Description is empty.
func New(cfg Config) *Agent {
	if strings.TrimSpace(cfg.Description) == "" {
		panic("agentsdk: Config.Description is required")
	}
	return newAgentRegistrationState(cfg)
}

func (a *Agent) initializeRuntime() {
	agentID := requireEnv("AIRLOCK_AGENT_ID")
	apiURL := requireEnv("AIRLOCK_API_URL")
	token := requireEnv("AIRLOCK_AGENT_TOKEN")
	dsn := requireEnv("AIRLOCK_DB_URL")

	sqlDB, err := sql.Open("postgres", dsn)
	if err != nil {
		panic("agentsdk: failed to open database: " + err.Error())
	}
	if err := pingDatabase(sqlDB); err != nil {
		_ = sqlDB.Close()
		panic("agentsdk: failed to connect to database: " + err.Error())
	}

	a.agentID = agentID
	a.apiURL = apiURL
	a.token = token
	a.httpClient = &http.Client{}
	a.db.db = sqlDB
	a.client = newAirlockClient(apiURL, token, a.httpClient)
	a.AddSensitive(token)
	a.runtimeInitialized()
	a.autoMigrate()
}

func newAgentRegistrationState(cfg Config) *Agent {
	a := &Agent{
		phase:            agentDefining,
		description:      cfg.Description,
		emoji:            cfg.Emoji,
		sensitiveSet:     make(map[string]struct{}),
		tools:            make(map[string]*registeredTool),
		agentDefinitions: make(map[string]*registeredAgent),
		webhooks:         make(map[string]*Webhook),
		jobs:             make(map[jobKey]*registeredJob),
		jobCrons:         make(map[string]*registeredJobCron),
		routes:           make(map[string]*Route),
		auths:            make(map[string]*Connection),
		mcps:             make(map[string]*MCP),
		envVars:          make(map[string]*EnvVar),
		topics:           make(map[string]*Topic),
		staticAssets:     make(map[string]*StaticAsset),
		connectors:       make(map[string]*Connector),
	}
	a.db = &AgentDB{agent: a}
	// Framework-owned scratch directory — used by run_js output truncation
	// and generated media. Builders may RegisterDirectory("tmp", ...); the
	// register helper preserves the framework's caps (the description may
	// still be supplied) so both sides share the same directory.
	a.directories = append(a.directories, &directory{
		Path:           reservedTmpPath,
		Read:           AccessUser,
		Write:          AccessUser,
		List:           AccessUser,
		Description:    "Ephemeral scratch (auto-managed by the framework — truncated tool output, generated media).",
		RetentionHours: 72, // sweeper drops files older than 3 days
	})
	// Inbox for files airlock places here on behalf of an external
	// caller through inline MCP uploads.
	// Its private provenance policy accepts exact user, conversation, or current
	// run segments without changing the meaning of public directory scopes.
	a.directories = append(a.directories, &directory{
		Path:               reservedIncomingPath,
		Read:               AccessAdmin,
		Write:              AccessAdmin,
		List:               AccessAdmin,
		Description:        "Inbound file scratch (framework-managed; per-scope reads, ephemeral).",
		RetentionHours:     24,
		incomingProvenance: true,
	})
	return a
}

func (a *Agent) runtimeUnavailable(operation string) error {
	return fmt.Errorf("agentsdk: %s is unavailable before the agent runtime starts", operation)
}

func (a *Agent) requireRuntime(operation string) {
	if !a.runtimeAvailable() {
		panic(a.runtimeUnavailable(operation).Error())
	}
}

func (a *Agent) runtimeAvailable() bool {
	a.phaseM.RLock()
	defer a.phaseM.RUnlock()
	return a.phase == agentStarting || a.phase == agentRunning
}

func (a *Agent) beginStart() error {
	a.phaseM.Lock()
	defer a.phaseM.Unlock()
	if a.phase != agentDefining {
		return errors.New("agentsdk: agent runtime can only start once")
	}
	a.phase = agentInitializing
	return nil
}

func (a *Agent) runtimeInitialized() {
	a.phaseM.Lock()
	defer a.phaseM.Unlock()
	if a.phase != agentInitializing {
		panic("agentsdk: invalid runtime initialization transition")
	}
	a.phase = agentStarting
}

func (a *Agent) finishStart() {
	a.phaseM.Lock()
	defer a.phaseM.Unlock()
	if a.phase != agentStarting {
		panic("agentsdk: invalid runtime start transition")
	}
	a.phase = agentRunning
}

func (a *Agent) markClosed() {
	a.phaseM.Lock()
	defer a.phaseM.Unlock()
	a.phase = agentClosed
}

// Logger returns the zap logger for the current handler invocation.
// Bind it once at handler entry — `log := a.Logger(ctx)` — and use it
// throughout; the ctx is consumed here to resolve the run, so callers
// don't thread it per line.
//
// When ctx carries a run, the returned logger is tagged with
// run_id/agent_id and tees every line two ways: structured JSON to
// container stdout (what an enterprise log pipeline scrapes) and a
// bounded per-run buffer that Airlock keeps as the run's log record
// (a failed run's copy also feeds the Fix-this-error builder). Outside
// a run (init, migrations, detached goroutines) it returns the plain
// stdout logger — no run to attach to.
//
// It is a real *zap.Logger: use zap.String/zap.Int/zap.Error/... for
// structured fields, and the level-named methods (Info/Warn/Error/Debug)
// for severity.
func (a *Agent) Logger(ctx context.Context) *zap.Logger {
	if r := a.runForCall(ctx); r != nil {
		return r.runLogger()
	}
	return agentLogger()
}

// DB returns the Agent's owned database pool.
// AgentDB implements the same DBTX interface that sqlc-generated New()
// takes, so `mygen.New(agent.DB())` works unchanged. The wrapper is the
// extension point through which the framework can later record query
// activity onto the run carried by ctx.
func (a *Agent) DB() *AgentDB {
	return a.db
}

// Close releases the database pool owned by a started Agent. Serve calls Close
// when it stops; tests that call Start directly should also close the Agent.
func (a *Agent) Close() error {
	if !a.runtimeAvailable() {
		return a.runtimeUnavailable("Close")
	}
	if a.db == nil || a.db.db == nil {
		return nil
	}
	err := a.db.db.Close()
	a.markClosed()
	return err
}

func pingDatabase(db *sql.DB) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil || !isTransientConnError(err) {
			return err
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
		}
	}
	return err
}

// applySyncResponse atomically stores the platform-supplied promptData
// + MCP discovery results + public storage base URL returned by an
// Airlock sync round-trip. Called both at startup (from
// syncWithAirlock in sync.go) and on /refresh.
//
// A zero-value promptData is an incompatible wire response. Panic so the
// operator sees the sync failure instead of a broken empty system prompt.
func (a *Agent) applySyncResponse(resp wire.SyncResponse) {
	if resp.PromptData.AgentRouteURL == "" {
		panic("agentsdk: applySyncResponse: empty promptData.agentRouteUrl; Airlock and agentsdk versions are incompatible")
	}
	validateSyncedBindings(resp)
	a.syncMu.Lock()
	a.promptData = resp.PromptData
	a.publicStorageBase = resp.PublicStorageBase
	a.syncMu.Unlock()
}

func validateSyncedBindings(resp wire.SyncResponse) {
	seenJS := make(map[string]string)
	seenDirect := make(map[string]string)
	check := func(owner string, paths map[string]binding.Path) {
		for canonical, path := range paths {
			if js := path.JS(); js != "" {
				if previous, ok := seenJS[js]; ok {
					panic(fmt.Sprintf("agentsdk: synced capability collision %q between %s and %s", js, previous, owner))
				}
				seenJS[js] = owner + "/" + canonical
			}
			direct := path.Direct()
			if previous, ok := seenDirect[direct]; ok {
				panic(fmt.Sprintf("agentsdk: synced capability collision %q between %s and %s", direct, previous, owner))
			}
			seenDirect[direct] = owner + "/" + canonical
		}
	}
	for slug, schemas := range resp.MCPSchemas {
		names := make([]string, len(schemas))
		for i, schema := range schemas {
			names[i] = schema.Name
		}
		paths, err := binding.External(binding.MCP, slug, slug, names)
		if err != nil {
			panic(fmt.Sprintf("agentsdk: synced MCP %q: %v", slug, err))
		}
		check("mcp "+slug, paths)
	}
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
