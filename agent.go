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
	webhooks map[string]webhookEntry
	crons    map[string]cronEntry
	auths         map[string]ConnectionDef
	mcps          map[string]MCPDef
	routes        map[string]routeEntry
	topics map[string]TopicDef

	extraPrompts []ExtraPromptSpec // access-scoped system prompt fragments; see AddExtraPrompt
	modelSlots   []ModelSlotDef    // named model slots; see RegisterModel

	systemPrompt string // rendered by Airlock during sync

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
		webhooks:     make(map[string]webhookEntry),
		crons:        make(map[string]cronEntry),
		auths:        make(map[string]ConnectionDef),
		mcps:         make(map[string]MCPDef),
		routes:        make(map[string]routeEntry),
		topics: make(map[string]TopicDef),
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
	return a
}

// Log records a message scoped to the current handler invocation. Visible
// in the Runs UI alongside the actions the handler performed.
func (a *Agent) Log(ctx context.Context, msg string) {
	if r := a.runForCall(ctx); r != nil {
		r.logAppend(msg)
		return
	}
	a.logger.Info(msg)
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

// --- Storage methods (S3 via Airlock) ---

// StoreFile stores a file in agent storage via Airlock.
func (a *Agent) StoreFile(ctx context.Context, key string, data io.Reader, contentType string) error {
	req, err := a.client.newRequest(ctx, "PUT", "/api/agent/storage/"+key, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentsdk: store file %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: store file %s: status %d: %s", key, resp.StatusCode, string(b))
	}
	return nil
}

// LoadFile loads a file from agent storage via Airlock.
func (a *Agent) LoadFile(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := a.client.do(ctx, "GET", "/api/agent/storage/"+key, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("agentsdk: load file %s: status %d", key, resp.StatusCode)
	}
	return resp.Body, nil
}

// DeleteFile deletes a file from agent storage via Airlock.
func (a *Agent) DeleteFile(ctx context.Context, key string) error {
	resp, err := a.client.do(ctx, "DELETE", "/api/agent/storage/"+key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: delete file %s: status %d: %s", key, resp.StatusCode, string(b))
	}
	return nil
}

// FileInfo returns metadata for a file in agent storage.
func (a *Agent) FileInfo(ctx context.Context, key string) (StoredFile, error) {
	body := struct {
		Key string `json:"key"`
	}{key}
	var info StoredFile
	if err := a.client.doJSON(ctx, "POST", "/api/agent/storage/info", body, &info); err != nil {
		return StoredFile{}, err
	}
	return info, nil
}

// CopyFile copies a file in agent storage via Airlock.
func (a *Agent) CopyFile(ctx context.Context, srcKey, dstKey string) error {
	body := struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}{srcKey, dstKey}
	return a.client.doJSON(ctx, "POST", "/api/agent/storage/copy", body, nil)
}

// ListFiles lists files in agent storage matching a prefix.
func (a *Agent) ListFiles(ctx context.Context, prefix string) ([]StoredFile, error) {
	path := "/api/agent/storage"
	if prefix != "" {
		path += "?prefix=" + prefix
	}
	var files []StoredFile
	if err := a.client.doJSON(ctx, "GET", path, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// GetAttachment retrieves a conversation file attachment.
func (a *Agent) GetAttachment(ctx context.Context, fileID string) (io.ReadCloser, error) {
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

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("agentsdk: required environment variable " + key + " is not set")
	}
	return v
}
