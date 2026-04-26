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

// WebhookOpts configures webhook verification and execution.
type WebhookOpts struct {
	Verify      string        `json:"verify"`      // "hmac", "token", "none" (default: "none")
	Header      string        `json:"header"`      // e.g. "X-Hub-Signature-256" (for hmac mode)
	Timeout     time.Duration `json:"-"`           // max execution time (default: 2 min)
	Description string        `json:"description"` // human-readable description
}

// webhookEntry holds a registered webhook handler and its options.
type webhookEntry struct {
	handler WebhookHandlerFunc
	opts    WebhookOpts
}

// CronHandlerFunc handles cron-triggered requests.
type CronHandlerFunc func(ctx context.Context, ew *EventWriter) error

// RouteHandlerFunc handles custom HTTP routes registered via RegisterRoute.
type RouteHandlerFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// CronOpts configures cron execution.
type CronOpts struct {
	Timeout     time.Duration // max execution time (default: 2 min)
	Description string        // human-readable description
}

// cronEntry holds a registered cron handler and its options.
type cronEntry struct {
	schedule string
	handler  CronHandlerFunc
	opts     CronOpts
}

// --- Connection definitions ---

// ConnectionDef defines an outgoing service connection registered with Airlock.
type ConnectionDef struct {
	Name              string        `json:"name"`
	Description       string        `json:"description"`
	AuthMode          string        `json:"authMode"`
	AuthURL           string        `json:"authUrl,omitempty"`
	TokenURL          string        `json:"tokenUrl,omitempty"`
	BaseURL           string        `json:"baseUrl,omitempty"`
	Scopes            []string      `json:"scopes,omitempty"`
	AuthInjection     AuthInjection `json:"authInjection"`
	SetupInstructions string        `json:"setupInstructions,omitempty"`
	LLMHint           string        `json:"llmHint,omitempty"` // injected into run_js tool description
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

// --- Topics ---

// TopicDef defines a topic that an agent can publish notifications to.
type TopicDef struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
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

// --- Route access levels ---

// Access defines who can reach an agent route.
type Access string

const (
	AccessAdmin  Access = "admin"
	AccessUser   Access = "user"
	AccessPublic Access = "public"
)

// RouteOpts configures a custom HTTP route.
type RouteOpts struct {
	Access      Access // required: AccessAdmin, AccessUser, or AccessPublic
	Description string // human-readable description (e.g. "Spotify control page")
}

// routeEntry holds a registered custom HTTP route.
type routeEntry struct {
	handler RouteHandlerFunc
	opts    RouteOpts
}

// --- MCP server definitions ---

// MCPDef defines an MCP server dependency registered with Airlock.
type MCPDef struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	AuthMode string   `json:"authMode"` // "oauth_discovery", "oauth", "token", "none"
	AuthURL  string   `json:"authUrl,omitempty"`
	TokenURL string   `json:"tokenUrl,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
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
	ExtraPrompts []ExtraPromptSpec `json:"extraPrompts,omitempty"`
	ModelSlots   []ModelSlotDef    `json:"modelSlots,omitempty"`
}

// ExtraPromptSpec is a single AddExtraPrompt fragment in the sync payload.
// Access is empty when the fragment applies to every access level.
type ExtraPromptSpec struct {
	Text   string   `json:"text"`
	Access []Access `json:"access,omitempty"`
}

// ModelSlotDef is a single named model slot declared via RegisterModel.
// The agent uses `Slug` at runtime (e.g. `agent.LLM(ctx, slug, ...)`);
// the admin binds a specific model to the slug in the Airlock UI. When no
// model is bound, calls fall through to the agent's per-capability default
// and then to the system default for that capability.
type ModelSlotDef struct {
	Slug        string `json:"slug"`
	Capability  string `json:"capability"`
	Description string `json:"description,omitempty"`
}

// ModelSlotOpts configures a RegisterModel call.
type ModelSlotOpts struct {
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

// ModelDef configures a model request. Used with run.LLM(), run.ImageModel(), etc.
type ModelDef struct {
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

