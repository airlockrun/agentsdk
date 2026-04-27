package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/airlockrun/goai/message"
)

// defaultTimeout is the default execution timeout for webhooks and crons.
const defaultTimeout = 2 * time.Minute

// --- Handler types ---

// WebhookHandlerFunc handles incoming webhook requests. Pass ctx to any
// agent.X(ctx, ...) call the body makes.
type WebhookHandlerFunc func(ctx context.Context, data []byte, ew *EventWriter) error

// CronHandlerFunc handles cron-triggered requests.
type CronHandlerFunc func(ctx context.Context, ew *EventWriter) error

// RouteHandlerFunc handles custom HTTP routes registered via RegisterRoute.
type RouteHandlerFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// --- Webhook ---

// Webhook is the self-contained declaration registered via agent.RegisterWebhook.
// Agents serve incoming HTTP at /webhook/{Path} on their container.
type Webhook struct {
	Path        string             // unique per agent
	Handler     WebhookHandlerFunc // required
	Verify      string             // "hmac" | "token" | "none" (default: "none")
	Header      string             // header carrying the signature/token (hmac/token modes)
	Timeout     time.Duration      // max execution time (default: 2 min)
	Description string
	Access      Access // who may invoke; default AccessUser
}

// --- Cron ---

// Cron is the self-contained declaration registered via agent.RegisterCron.
// Crons fire by schedule, never by user action — no Access field.
type Cron struct {
	Name        string          // unique per agent
	Schedule    string          // standard cron expression, e.g. "0 9 * * *"
	Handler     CronHandlerFunc // required
	Timeout     time.Duration   // max execution time (default: 2 min)
	Description string
}

// --- Route ---

// Route is the self-contained declaration registered via agent.RegisterRoute.
// Custom HTTP routes served by the agent and proxied by Airlock via subdomain
// routing. The (Method, Path) pair must be unique per agent.
type Route struct {
	Method      string           // "GET", "POST", ...
	Path        string           // e.g. "/spotify"
	Handler     RouteHandlerFunc // required
	Access      Access           // required: AccessAdmin, AccessUser, or AccessPublic
	Description string
}

// --- Connection ---

// Connection is the self-contained declaration registered via
// agent.RegisterConnection — an outgoing service Airlock proxies for the agent
// with credentials it manages.
type Connection struct {
	Slug              string        `json:"-"` // unique per agent; binds as conn_{slug} in run_js — sent in URL, not body
	Name              string        `json:"name"`
	Description       string        `json:"description"`
	BaseURL           string         `json:"baseUrl,omitempty"`
	AuthMode          ConnectionAuth `json:"authMode"`
	AuthURL           string        `json:"authUrl,omitempty"`
	TokenURL          string        `json:"tokenUrl,omitempty"`
	Scopes            []string      `json:"scopes,omitempty"`
	AuthInjection     AuthInjection `json:"authInjection"`
	SetupInstructions string        `json:"setupInstructions,omitempty"`
	LLMHint           string        `json:"llmHint,omitempty"` // appended to the connection block in the system prompt
	Access            Access        `json:"access,omitempty"`  // who may invoke conn_{slug}; default AccessUser
}

// AuthInjection defines how auth credentials are injected into proxied requests.
type AuthInjection struct {
	Type string `json:"type"` // "bearer", "api_key_header", "bot_token_url_prefix"
	Name string `json:"name,omitempty"` // header name for api_key_header (default: "X-API-Key")
}

// --- Run recording ---

// Action records a single operation performed during a Run.
type Action struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Duration  int64     `json:"durationMs"`
	Request   any       `json:"request,omitempty"`
	Response  any       `json:"response,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// --- Storage ---

// StoredFile describes a file in agent storage.
type StoredFile struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ContentType  string    `json:"contentType"`
	LastModified time.Time `json:"lastModified"`
}

// --- Auth errors ---

// AuthRequiredError is returned by ConnectionHandle.Request when a connection needs authorization.
type AuthRequiredError struct {
	Slug     string `json:"slug"`
	ConnName string `json:"connName"`
	AuthURL  string `json:"authUrl"`
}

func (e *AuthRequiredError) Error() string {
	return fmt.Sprintf("authorization required for %s: visit %s", e.ConnName, e.AuthURL)
}

// IsAuthRequired checks whether err is an *AuthRequiredError.
func IsAuthRequired(err error) (*AuthRequiredError, bool) {
	var ae *AuthRequiredError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// --- Prompt input ---

// PromptInput is the request body for POST /prompt.
type PromptInput struct {
	Messages        []message.Message `json:"messages"`
	Message         string            `json:"message,omitempty"` // New user message text (used with SessionStore)
	ConversationID  string            `json:"conversationId,omitempty"`
	ProviderID      string            `json:"providerId,omitempty"`
	ModelID         string            `json:"modelId,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxOutputTokens *int              `json:"maxOutputTokens,omitempty"`
	ProviderOptions json.RawMessage   `json:"providerOptions,omitempty"`
	Files           []FileRef         `json:"files,omitempty"`
	ResumeRunID         string            `json:"resumeRunId,omitempty"`
	Approved            *bool             `json:"approved,omitempty"`
	SupportedModalities []string          `json:"supportedModalities,omitempty"` // e.g. ["text", "image", "pdf", "audio", "video"]
	Source              string            `json:"source,omitempty"`              // "user" (default), "system" (injected by Airlock)

	// ExtraSystemPrompt is an access-filtered concatenation of the agent's
	// registered AddExtraPrompt fragments, composed by Airlock at run
	// dispatch. The agent appends this to its sync-cached system prompt.
	ExtraSystemPrompt string `json:"extraSystemPrompt,omitempty"`

	// CallerAccess is the resolved per-(agent, user) access level for the
	// triggering caller. agentsdk uses it to gate which conn_/mcp_/topic_/
	// storage_ JS bindings (and registered tools) are exposed to the run.
	// Airlock sets this from trigger.ResolveAgentAccess. For trusted server
	// triggers (webhooks, crons) Airlock sends AccessAdmin.
	CallerAccess Access `json:"callerAccess,omitempty"`

	// ForceCompact tells the agent to skip the thinking loop and run a
	// user-triggered compaction instead. Message is ignored when set. The
	// agent loads conversation history, asks the model to summarize it,
	// persists the summary via the SessionStore's Compact method, and emits
	// a short text-delta describing the outcome.
	ForceCompact bool `json:"forceCompact,omitempty"`
}

// FileRef references a file attachment in a prompt.
type FileRef struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// --- Storage ---

// Storage is the self-contained declaration registered via agent.RegisterStorage.
// Each zone owns an S3 prefix ("{Slug}/") and a JS binding (storage_{slug});
// AccessInternal axes are reachable only from builder Go code and never
// surface in run_js. The framework auto-registers a reserved zone "tmp"
// at Read=Write=AccessUser; builder calls with Slug="tmp" silently no-op
// (returning the framework handle) so frameworks and builders share the
// same scratch area without conflict.
//
// Read and Write are independent — "Read: AccessUser, Write: AccessAdmin"
// (admin-curated, user-readable) and "Read: AccessAdmin, Write: AccessUser"
// (user-fed inbox processed only by admins) are both valid. The JS
// `storage_{slug}` object exposes Get/Stat/List only when Read satisfies
// the caller, and Put/Delete/Copy only when Write does.
type Storage struct {
	Slug        string
	Read        Access // gates Get/Stat/List + the public route; default AccessUser
	Write       Access // gates Put/Delete/Copy/CopyTo; default AccessUser
	Description string // shown in the system prompt's storage zones section
}

// StorageZoneDef is the wire format sent in SyncRequest.
type StorageZoneDef struct {
	Slug        string `json:"slug"`
	Read        string `json:"read"`
	Write       string `json:"write"`
	Description string `json:"description"`
}

// --- Topic ---

// Topic is the self-contained declaration registered via agent.RegisterTopic.
// Conversations subscribe to a topic via topic_{slug}.subscribe() in run_js;
// builders publish via the *TopicHandle returned by RegisterTopic.
type Topic struct {
	Slug        string
	Description string
	Access      Access // who may subscribe via topic_{slug}.subscribe(); default AccessUser
}

// TopicDef is the wire format sent in SyncRequest.
type TopicDef struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Access      string `json:"access"`
}

// --- Display parts (printToUser / topic publish) ---

// DisplayPart is a single piece of rich content for user-facing output.
// Used by both printToUser (VM) and TopicHandle.Publish (Go).
type DisplayPart struct {
	Type     string  `json:"type"`                    // "text", "image", "file", "audio", "video"
	Text     string  `json:"text,omitempty"`           // body text, or caption for media types
	Source   string  `json:"source,omitempty"`          // S3 key
	URL      string  `json:"url,omitempty"`             // external URL
	Data     []byte  `json:"data,omitempty"`            // raw bytes (base64 in JSON)
	Filename string  `json:"filename,omitempty"`
	MimeType string  `json:"mimeType,omitempty"`
	Alt      string  `json:"alt,omitempty"`             // accessibility text for images
	Duration float64 `json:"duration,omitempty"`        // seconds, audio/video
}

// PrintRequest is the body for POST /api/agent/print.
type PrintRequest struct {
	Parts          []DisplayPart `json:"parts"`
	Topic          string        `json:"topic,omitempty"`          // empty = direct to conversation
	ConversationID string        `json:"conversationId,omitempty"` // set for direct prints
	RunID          string        `json:"runId,omitempty"`          // originating run, used to sort ephemerals after their run's assistant messages
}

// resolveDisplayPart infers missing fields on a DisplayPart from available data.
// 1. If Data is set but MimeType is empty → detect from bytes.
// 2. If MimeType is set but Type is empty → infer from MIME prefix.
// 3. If Filename is empty and part has media → generate from type + mimeType.
func ResolveDisplayPart(p *DisplayPart) {
	// Infer MimeType from raw bytes.
	if len(p.Data) > 0 && p.MimeType == "" {
		p.MimeType = http.DetectContentType(p.Data)
	}

	// Infer Type from MimeType.
	if p.Type == "" && p.MimeType != "" {
		switch {
		case strings.HasPrefix(p.MimeType, "image/"):
			p.Type = "image"
		case strings.HasPrefix(p.MimeType, "audio/"):
			p.Type = "audio"
		case strings.HasPrefix(p.MimeType, "video/"):
			p.Type = "video"
		default:
			p.Type = "file"
		}
	}

	// Default type for text-only parts.
	if p.Type == "" && p.Text != "" {
		p.Type = "text"
	}

	// Generate filename for media parts without one.
	if p.Filename == "" && p.Type != "" && p.Type != "text" {
		ext := ".bin"
		if exts, _ := mime.ExtensionsByType(p.MimeType); len(exts) > 0 {
			ext = exts[0]
		}
		p.Filename = p.Type + ext
	}
}

// --- Access levels ---

// Access defines who can reach a tool, connection, MCP, topic, or storage zone.
type Access string

const (
	AccessAdmin  Access = "admin"
	AccessUser   Access = "user"
	AccessPublic Access = "public"
	// AccessInternal is the strictest level — builder Go code only. Items
	// registered with AccessInternal are never exposed to the JS runtime
	// and are never reachable from external callers regardless of role.
	// Use it when you have, say, a storage zone that holds builder-only
	// caches you don't want the LLM to discover, mutate, or list.
	AccessInternal Access = "internal"
)

// --- Auth modes ---

// ConnectionAuth enumerates the supported authentication strategies for an
// outgoing service Connection.
type ConnectionAuth string

const (
	ConnectionAuthOAuth ConnectionAuth = "oauth"
	ConnectionAuthToken ConnectionAuth = "token"
	ConnectionAuthNone  ConnectionAuth = "none"
)

// MCPAuth enumerates the supported authentication strategies for an MCP
// server. MCPAuthOAuthDiscovery is MCP-specific (RFC 9728 server-advertised
// OAuth endpoints) and not available on Connection.
type MCPAuth string

const (
	MCPAuthOAuth          MCPAuth = "oauth"
	MCPAuthOAuthDiscovery MCPAuth = "oauth_discovery"
	MCPAuthToken          MCPAuth = "token"
	MCPAuthNone           MCPAuth = "none"
)

// --- MCP ---

// MCP is the self-contained declaration registered via agent.RegisterMCP.
// Slug binds as mcp_{slug} in run_js; the builder uses the returned *MCPHandle
// to call tools from Go.
type MCP struct {
	Slug     string   `json:"-"` // sent in URL, not body
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	AuthMode MCPAuth  `json:"authMode"`
	AuthURL  string   `json:"authUrl,omitempty"`
	TokenURL string   `json:"tokenUrl,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	Access   Access   `json:"access,omitempty"` // who may invoke mcp_{slug}; default AccessUser
}

// MCPServerSync is the MCP server definition sent in SyncRequest.
type MCPServerSync struct {
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	AuthMode string   `json:"authMode"`
	AuthURL  string   `json:"authUrl,omitempty"`
	TokenURL string   `json:"tokenUrl,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	Access   string   `json:"access"`
}

// MCPToolSchema is a discovered MCP tool schema returned in SyncResponse.
type MCPToolSchema struct {
	ServerSlug  string          `json:"serverSlug"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPAuthStatus reports auth state for an MCP server.
type MCPAuthStatus struct {
	Slug       string `json:"slug"`
	AuthMode   string `json:"authMode"`
	Authorized bool   `json:"authorized"`
	AuthURL    string `json:"authUrl,omitempty"`
}

// MCPToolCallRequest is the body for POST /api/agent/mcp/{slug}/tools/call.
type MCPToolCallRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

// MCPToolCallResponse is returned from MCP tool call proxy.
type MCPToolCallResponse struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

// MCPContent is a single content block in an MCP tool response.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// --- Sync / wire types (shared between agentsdk client and airlock server) ---

// SyncRequest is the body for PUT /api/agent/sync.
type SyncRequest struct {
	Version      string            `json:"version"`
	Description  string            `json:"description,omitempty"`
	Tools        []SyncToolDef     `json:"tools,omitempty"`
	Webhooks     []WebhookDef      `json:"webhooks"`
	Crons        []CronEntry       `json:"crons"`
	Routes       []RouteDef        `json:"routes,omitempty"`
	Topics       []TopicDef        `json:"topics,omitempty"`
	MCPServers   []MCPServerSync   `json:"mcpServers,omitempty"`
	Storages     []StorageZoneDef  `json:"storages,omitempty"`
	ExtraPrompts []ExtraPromptSpec `json:"extraPrompts,omitempty"`
	ModelSlots   []ModelSlotDef    `json:"modelSlots,omitempty"`
}

// ExtraPrompt is the self-contained declaration passed to agent.AddExtraPrompt.
// The Text fragment is appended to the system prompt for runs whose caller
// access matches one of the listed Access levels. Empty Access slice means
// "applies to every access level."
type ExtraPrompt struct {
	Text   string
	Access []Access
}

// ExtraPromptSpec is the wire format sent in SyncRequest.
type ExtraPromptSpec struct {
	Text   string   `json:"text"`
	Access []Access `json:"access,omitempty"`
}

// ModelSlotDef is the wire format sent in SyncRequest. The agent uses Slug
// at runtime (e.g. `agent.LLM(ctx, slug, ...)`); the admin binds a specific
// model to the slug in the Airlock UI. When no model is bound, calls fall
// through to the agent's per-capability default and then to the system
// default for that capability.
type ModelSlotDef struct {
	Slug        string `json:"slug"`
	Capability  string `json:"capability"`
	Description string `json:"description,omitempty"`
}

// ModelSlot is the self-contained declaration registered via agent.RegisterModel.
type ModelSlot struct {
	Slug        string
	Capability  ModelCapability // required: CapText, CapVision, CapImage, CapSpeech, CapTranscription, CapEmbedding
	Description string          // human-readable hint shown in the admin UI
}

// SyncResponse is the response from PUT /api/agent/sync.
type SyncResponse struct {
	SystemPrompt  string          `json:"systemPrompt"`
	MCPAuthStatus []MCPAuthStatus `json:"mcpAuthStatus,omitempty"`
	// MCPSchemas carries discovered tool schemas per MCP server slug.
	// Airlock populates these from its server-side discovery cache so the
	// agent's VM can install one typed JS method per tool on each
	// `mcp_{slug}` object — no per-run discovery round-trips.
	MCPSchemas map[string][]MCPToolSchema `json:"mcpSchemas,omitempty"`
	// PublicStorageBase is the URL prefix at which storage zones are reachable
	// on the agent's subdomain, ending without a trailing slash;
	// *StorageHandle.URL appends "/{slug}/{key}". Of the form
	// https://{slug}.{agentDomain}/__air/storage. The proxy enforces the
	// zone's Read access at fetch time — public zones serve unauthenticated,
	// user/admin zones require subdomain login (redirect-on-missing-cookie).
	PublicStorageBase string `json:"publicStorageBase,omitempty"`
}

// SyncToolDef describes a registered tool sent during sync. Carries the
// JSON schemas for input and output so Airlock can render TypeScript
// signatures in the system prompt and surface them in the UI.
type SyncToolDef struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Access        string            `json:"access"` // "admin", "user", "public"
	InputSchema   json.RawMessage   `json:"inputSchema,omitempty"`
	OutputSchema  json.RawMessage   `json:"outputSchema,omitempty"`
	InputExamples []json.RawMessage `json:"inputExamples,omitempty"`
}

// RouteDef is a custom HTTP route definition sent during sync.
type RouteDef struct {
	Path        string `json:"path"`
	Method      string `json:"method"`
	Access      string `json:"access"`
	Description string `json:"description,omitempty"`
}

// WebhookDef is a webhook definition sent during sync.
type WebhookDef struct {
	Path        string `json:"path"`
	Verify      string `json:"verify"`
	Header      string `json:"header,omitempty"`
	TimeoutMs   int64  `json:"timeoutMs"`
	Description string `json:"description,omitempty"`
}

// CronEntry is a cron job definition sent during sync.
type CronEntry struct {
	Name        string `json:"name"`
	Schedule    string `json:"schedule"`
	TimeoutMs   int64  `json:"timeoutMs"`
	Description string `json:"description,omitempty"`
}

// HTTPRequest is the body for POST /api/agent/http.
type HTTPRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`  // default: GET
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // seconds, default: 30, max: 120
	SaveAs  string            `json:"saveAs,omitempty"`  // save response body to S3 at this key (binary-safe)
	Raw     bool              `json:"raw,omitempty"`     // skip HTML→markdown conversion for HTML responses
}

// HTTPResponse is returned from POST /api/agent/http.
type HTTPResponse struct {
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"contentType"` // original upstream Content-Type
	Size        int               `json:"size"`
	SavedTo     string            `json:"savedTo,omitempty"` // S3 key if body was auto-saved
	Note        string            `json:"note,omitempty"`    // human-readable note about transformations applied (e.g. HTML→markdown conversion)
}

// ProxyRequest is the body for POST /api/agent/proxy/{slug}.
type ProxyRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
}

// --- Model capability types ---

// ModelCapability describes what kind of model is needed.
type ModelCapability string

const (
	CapText          ModelCapability = "text"           // any chat/language model
	CapVision        ModelCapability = "vision"          // chat model that accepts images
	CapEmbedding     ModelCapability = "embedding"       // vector embeddings
	CapImage         ModelCapability = "image"            // image generation
	CapSpeech        ModelCapability = "speech"           // text-to-speech
	CapTranscription ModelCapability = "transcription"    // speech-to-text
)

// ModelOpts configures a model request. Used with agent.LLM(), agent.ImageModel(), etc.
type ModelOpts struct {
	// Capability selects the model sub-type. Only meaningful for run.LLM()
	// (distinguishes text vs vision). For other methods, the method name
	// determines the capability and this field is ignored.
	Capability ModelCapability `json:"capability,omitempty"`

	// Description is optional human-readable context for run logs/UI.
	Description string `json:"description,omitempty"`
}

// LLMProxyRequest is the body for POST /api/agent/llm/stream.
type LLMProxyRequest struct {
	Slug       string          `json:"slug,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Options    json.RawMessage `json:"options"`
}

// ModelProxyRequest is the body for non-streaming model endpoints
// (POST /api/agent/llm/{image,embedding,speech,transcription}).
type ModelProxyRequest struct {
	Slug       string          `json:"slug,omitempty"`
	Capability string          `json:"capability"`
	Options    json.RawMessage `json:"options"`
}

// CreateRunRequest is the body for POST /api/agent/run/create.
type CreateRunRequest struct {
	TriggerType string `json:"triggerType"`
	TriggerRef  string `json:"triggerRef"`
}

// CreateRunResponse is the response from POST /api/agent/run/create.
type CreateRunResponse struct {
	RunID string `json:"runId"`
}

// RunCompleteRequest is the body for POST /api/agent/run/complete.
type RunCompleteRequest struct {
	RunID      string          `json:"runId"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	PanicTrace string          `json:"panicTrace,omitempty"`
	Actions    json.RawMessage `json:"actions"`
	Logs       []string        `json:"logs,omitempty"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
}

