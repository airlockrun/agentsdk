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

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/prompt"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
	_ "github.com/lib/pq" // register "postgres" driver for agent.DB()
	"go.uber.org/zap"
)

// Config holds configuration for creating an Agent.
type Config struct {
	Description string // required — shown to users in the Airlock UI
	// Emoji is an optional decorative glyph shown next to the agent in
	// the Airlock UI (agent list, sidebar, header). Purely cosmetic;
	// empty means "no emoji". A short grapheme is expected (a single
	// emoji incl. ZWJ / skin-tone / flag sequences) — it is NOT
	// validated to one rune; over-long/garbage values are dropped
	// server-side rather than failing the sync.
	Emoji string
}

// Agent is a long-lived singleton, one per container.
// Created once at startup via New(), lives for the lifetime of the process.
type Agent struct {
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
	webhooks         map[string]*Webhook
	scheduleHandlers map[string]*scheduleHandler
	routes           map[string]*Route
	auths            map[string]*Connection
	mcps             map[string]*MCP
	envVars          map[string]*EnvVar
	topics           map[string]*Topic
	execEndpoints    map[string]*ExecEndpoint
	staticAssets     map[string]*StaticAsset
	directories      []*Directory // registration order; longest-prefix wins at lookup

	instructions []*Instruction // access-scoped system prompt fragments; see AddInstruction
	modelSlots   []*ModelSlot   // named model slots; see RegisterModel

	// Airlock-owned state: discovered server-side at sync time and pushed
	// back via syncResponse. /refresh re-runs sync to pick up changes
	// (e.g. MCP OAuth completion) without restarting the container.
	syncMu            sync.RWMutex
	promptData        wire.PromptData                 // platform-supplied prompt inputs (siblings, URLs); filled by applySyncResponse
	mcpAuthStatus     []wire.MCPAuthStatus            // per-server auth status (for prompt status lines)
	mcpSchemas        map[string][]wire.MCPToolSchema // server slug → discovered tools
	publicStorageBase string                          // base URL for AccessPublic zone reads (subdomain or host-level fallback)
	syncStateHash     string                          // airlock's config fingerprint at last sync; drift vs a dispatch's ExpectedSyncHash triggers a self-heal resync

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

// UserFromContext returns the human a run is acting for. The second return is
// false for runs with no originating user — cron/schedule/webhook triggers and
// anonymous/public prompt runs. ID is the stable internal-user uuid and is the
// key to scope agent-owned data by; Email/DisplayName are display claims.
// Reading it never materializes a run, so route handlers can call it freely:
// /prompt and route runs carry id+email+display name (airlock forwards
// X-User-ID/Email/Name); the A2A path carries id only.
func UserFromContext(ctx context.Context) (User, bool) {
	if r := runFromContext(ctx); r != nil {
		if r.userID == "" {
			return User{}, false
		}
		return User{ID: r.userID, Email: r.userEmail, DisplayName: r.userDisplayName}, true
	}
	if l := lazyRunFromContext(ctx); l != nil {
		if l.userID == "" {
			return User{}, false
		}
		return User{ID: l.userID, Email: l.userEmail, DisplayName: l.userDisplayName}, true
	}
	if test, ok := testcaller.FromContext(ctx); ok && test.UserID != "" {
		return User{ID: test.UserID, Email: test.Email, DisplayName: test.DisplayName}, true
	}
	return User{}, false
}

// New creates an Agent by reading required environment variables.
// Panics if AIRLOCK_AGENT_ID, AIRLOCK_API_URL, AIRLOCK_AGENT_TOKEN, or
// AIRLOCK_DB_URL is missing. The database connection is opened, checked, and
// migrated before New returns.
// Panics if Config.Description is empty.
func New(cfg Config) *Agent {
	if strings.TrimSpace(cfg.Description) == "" {
		panic("agentsdk: Config.Description is required")
	}

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

	a := &Agent{
		agentID:          agentID,
		apiURL:           apiURL,
		token:            token,
		description:      cfg.Description,
		emoji:            cfg.Emoji,
		httpClient:       &http.Client{},
		db:               &AgentDB{db: sqlDB},
		sensitiveSet:     make(map[string]struct{}),
		tools:            make(map[string]*registeredTool),
		webhooks:         make(map[string]*Webhook),
		scheduleHandlers: make(map[string]*scheduleHandler),
		routes:           make(map[string]*Route),
		auths:            make(map[string]*Connection),
		mcps:             make(map[string]*MCP),
		envVars:          make(map[string]*EnvVar),
		topics:           make(map[string]*Topic),
		execEndpoints:    make(map[string]*ExecEndpoint),
		staticAssets:     make(map[string]*StaticAsset),
	}
	a.db.agent = a
	a.client = newAirlockClient(apiURL, token, a.httpClient)
	a.AddSensitive(token)
	func() {
		defer func() {
			if v := recover(); v != nil {
				_ = sqlDB.Close()
				panic(v)
			}
		}()
		a.autoMigrate()
	}()
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
	// A2A inbox: airlock copies file args from sibling callers into
	// agents/{this}/__a2a/{callerRun}/... before forwarding the tool
	// call. Tool bodies read it transparently — the path arrives in
	// the arg, not via any direct fileRead of "__a2a/...". Admin-only
	// because nobody should be poking at it from JS.
	// Inbox for files airlock places here on behalf of an external
	// caller (A2A tool args, prompt-meta files, inline MCP uploads).
	// Base ACL is locked admin/admin/admin — the scoped-directory
	// overlay in CheckFileAccess grants read access only to the
	// specific run / conversation / user that owns the sub-path. This
	// keeps anonymous and cross-caller traffic isolated even when
	// both arrive at a public-mcp agent. Scope=ScopeRun picks the
	// strictest available key when writing (airlock controls writes,
	// not WriteFile); the read overlay still accepts any of
	// user-/conv-/run- prefixes the path actually carries.
	a.directories = append(a.directories, &Directory{
		Path:           reservedIncomingPath,
		Read:           AccessAdmin,
		Write:          AccessAdmin,
		List:           AccessAdmin,
		Description:    "Inbound file scratch (framework-managed; per-scope reads, ephemeral).",
		RetentionHours: 24,
		Scope:          ScopeRun,
	})
	// Outbox: airlock copies file results returned from sibling
	// agents into agents/{this}/siblings/<sibling-slug>/<path>.
	// Caller's run_js can fileRead() these naturally; longer retention
	// than the inbox because the caller may want to keep working with
	// the file across follow-up turns.
	a.directories = append(a.directories, &Directory{
		Path:           reservedSiblingsPath,
		Read:           AccessUser,
		Write:          AccessUser,
		List:           AccessUser,
		Description:    "Files returned by sibling agents (framework-managed; cleaned after 3 days).",
		RetentionHours: 72,
	})
	return a
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

// Close releases the database pool owned by the Agent. Serve calls Close when
// it stops; tests that construct an Agent with New should also close it.
func (a *Agent) Close() error {
	if a.db == nil {
		return nil
	}
	return a.db.db.Close()
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

// renderSystemPrompt builds the per-run system prompt from the agent's
// live registrations and platform-supplied promptData. On-demand rendering
// expresses per-user sibling visibility.
//
// caller is the resolved access level for the run; visibleSiblings is
// the set of sibling IDs this run's user can A2A-call (uuid.Nil
// excluded). Pass nil to disable the sibling section entirely (e.g.
// cron/webhook runs with no original user).
//
// Unknown caller access values panic — they can only happen via a
// wire-shape bug, and silently mapping would mask it.
// promptEnv is the per-turn environment rendered into the prompt's <env>
// block. Every field is set explicitly by the caller (never inferred); an
// empty field is simply omitted from the block.
type promptEnv struct {
	Date         string
	Platform     string
	UserName     string
	UserEmail    string
	Conversation string
}

func (a *Agent) renderSystemPrompt(caller Access, visibleSiblings []uuid.UUID, env promptEnv, directTools bool) string {
	switch caller {
	case AccessAdmin, AccessUser, AccessPublic, "":
		// ok
	default:
		panic("agentsdk: renderSystemPrompt: unknown caller access " + string(caller))
	}
	data := a.buildPromptData(caller, visibleSiblings)
	data.Date = env.Date
	data.Platform = env.Platform
	data.UserName = env.UserName
	data.UserEmail = env.UserEmail
	data.Conversation = env.Conversation
	data.DirectTools = directTools
	tier := string(caller)
	if tier == "" {
		tier = string(AccessUser)
	}
	out, err := prompt.Render(data, tier)
	if err != nil {
		// Render errors here are template bugs, not user input — panic
		// loud so the operator notices in test rather than shipping a
		// silently-broken prompt.
		panic("agentsdk: renderSystemPrompt: " + err.Error())
	}
	return out
}

// snapshotMCPSchemas returns a value-copy of the MCP schema map. Callers
// (e.g. vm.go) work against the snapshot for the duration of a run so a
// concurrent /refresh can't mutate the map mid-iteration.
func (a *Agent) snapshotMCPSchemas() map[string][]wire.MCPToolSchema {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	if a.mcpSchemas == nil {
		return nil
	}
	out := make(map[string][]wire.MCPToolSchema, len(a.mcpSchemas))
	for k, v := range a.mcpSchemas {
		out[k] = v
	}
	return out
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
	a.syncMu.Lock()
	a.promptData = resp.PromptData
	a.mcpAuthStatus = resp.MCPAuthStatus
	a.mcpSchemas = resp.MCPSchemas
	a.publicStorageBase = resp.PublicStorageBase
	a.syncStateHash = resp.SyncStateHash
	a.syncMu.Unlock()
}

// syncedStateHash returns airlock's config fingerprint as of the last
// applied sync. A dispatch whose ExpectedSyncHash differs means the agent's
// cached promptData is stale; the /prompt path resyncs to catch up.
func (a *Agent) syncedStateHash() string {
	a.syncMu.RLock()
	defer a.syncMu.RUnlock()
	return a.syncStateHash
}

// buildPromptData assembles prompt.AgentData from the agent's
// in-memory registrations and the platform's promptData. The caller holds
// no locks; we grab syncMu.RLock internally.
//
// caller filtering happens inside prompt.Render — we just hand it
// every tool/conn/etc. registered with the agent. Sibling visibility
// is per-user (not per-tier) so we intersect promptData.Siblings
// with visibleSiblings here.
func (a *Agent) buildPromptData(caller Access, visibleSiblings []uuid.UUID) prompt.AgentData {
	a.syncMu.RLock()
	pd := a.promptData
	auth := append([]wire.MCPAuthStatus(nil), a.mcpAuthStatus...)
	schemas := make(map[string][]wire.MCPToolSchema, len(a.mcpSchemas))
	for k, v := range a.mcpSchemas {
		schemas[k] = v
	}
	a.syncMu.RUnlock()

	tools := make([]prompt.ToolInfo, 0, len(a.tools))
	for _, t := range a.tools {
		tools = append(tools, prompt.ToolInfo{
			Name:         t.Name,
			Description:  t.Description,
			LLMHint:      t.llmHint,
			Access:       string(t.access),
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
		})
	}

	conns := make([]prompt.ConnInfo, 0, len(a.auths))
	for _, c := range a.auths {
		conns = append(conns, prompt.ConnInfo{
			Slug:        c.Slug,
			Name:        c.Name,
			Description: c.Description,
			LLMHint:     c.LLMHint,
			BaseURL:     c.BaseURL,
			Access:      string(c.Access),
		})
	}

	execEndpoints := make([]prompt.ExecEndpointInfo, 0, len(a.execEndpoints))
	for _, e := range a.execEndpoints {
		execEndpoints = append(execEndpoints, prompt.ExecEndpointInfo{
			Slug:        e.Slug,
			Description: e.Description,
			LLMHint:     e.LLMHint,
			Access:      string(e.Access),
		})
	}

	topics := make([]prompt.TopicInfo, 0, len(a.topics))
	for _, t := range a.topics {
		topics = append(topics, prompt.TopicInfo{
			Slug:        t.Slug,
			Description: t.Description,
			LLMHint:     t.LLMHint,
			Access:      string(t.Access),
		})
	}

	webhooks := make([]prompt.WebhookInfo, 0, len(a.webhooks))
	for _, w := range a.webhooks {
		webhooks = append(webhooks, prompt.WebhookInfo{
			Path:        w.Path,
			Description: w.Description,
		})
	}

	routes := make([]prompt.RouteInfo, 0, len(a.routes))
	for _, r := range a.routes {
		routes = append(routes, prompt.RouteInfo{
			Method:      r.Method,
			Path:        r.Path,
			Access:      string(r.Access),
			Description: r.Description,
		})
	}

	dirs := make([]prompt.DirInfo, 0, len(a.directories))
	for _, d := range a.directories {
		dirs = append(dirs, prompt.DirInfo{
			Path:        d.Path,
			Description: d.Description,
			LLMHint:     d.LLMHint,
			Read:        string(d.Read),
			Write:       string(d.Write),
			List:        string(d.List),
			Scope:       string(d.Scope),
		})
	}

	mcpServers := make([]prompt.MCPServerStatus, 0, len(a.mcps))
	authBySlug := make(map[string]wire.MCPAuthStatus, len(auth))
	for _, s := range auth {
		authBySlug[s.Slug] = s
	}
	for _, m := range a.mcps {
		status := "requires authentication"
		var tools []prompt.ToolInfo
		if s, ok := authBySlug[m.Slug]; ok && s.Authorized {
			schema := schemas[m.Slug]
			status = fmt.Sprintf("connected, %d tools", len(schema))
			tools = make([]prompt.ToolInfo, len(schema))
			for i, t := range schema {
				tools[i] = prompt.ToolInfo{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				}
			}
		}
		mcpServers = append(mcpServers, prompt.MCPServerStatus{
			Slug:        m.Slug,
			Name:        m.Name,
			Status:      status,
			Access:      string(m.Access),
			Description: authBySlug[m.Slug].Instructions,
			Tools:       tools,
		})
	}

	// Per-user sibling visibility: intersect synced address book with
	// the visible set passed in. If visibleSiblings is nil (cron /
	// webhook runs, no original user) the Siblings section is omitted
	// entirely so the LLM doesn't see bindings it can't invoke.
	var siblings []prompt.SiblingInfo
	if len(visibleSiblings) > 0 && len(pd.Siblings) > 0 {
		visible := make(map[uuid.UUID]struct{}, len(visibleSiblings))
		for _, id := range visibleSiblings {
			visible[id] = struct{}{}
		}
		for _, s := range pd.Siblings {
			if _, ok := visible[s.ID]; !ok {
				continue
			}
			tools := make([]prompt.ToolInfo, len(s.Tools))
			for i, t := range s.Tools {
				tools[i] = prompt.ToolInfo{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				}
			}
			siblings = append(siblings, prompt.SiblingInfo{
				ID:          s.ID,
				Slug:        s.Slug,
				Name:        s.Name,
				Description: s.Description,
				Tools:       tools,
			})
		}
	}

	modalities := pd.SupportedModalities

	return prompt.AgentData{
		AgentDashboardURL: pd.AgentDashboardURL,
		AgentRouteURL:     pd.AgentRouteURL,
		Capabilities: prompt.Capabilities{
			Vision:        pd.Capabilities.Vision,
			Transcription: pd.Capabilities.Transcription,
			Speech:        pd.Capabilities.Speech,
			Embedding:     pd.Capabilities.Embedding,
			Image:         pd.Capabilities.Image,
			Search:        pd.Capabilities.Search,
		},
		SupportedModalities: prompt.Modalities(modalities),
		Tools:               tools,
		Connections:         conns,
		Topics:              topics,
		Webhooks:            webhooks,
		Routes:              routes,
		MCPServers:          mcpServers,
		Siblings:            siblings,
		Directories:         dirs,
		ExecEndpoints:       execEndpoints,
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
