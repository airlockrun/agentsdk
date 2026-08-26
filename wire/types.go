// Package wire defines the internal JSON protocol exchanged by agent runtimes
// and Airlock. It is not the author-facing Agents SDK API.
package wire

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/sol/session"
	"github.com/airlockrun/sol/websearch"
	"github.com/google/uuid"
)

type Access string

const (
	AccessAdmin  Access = "admin"
	AccessUser   Access = "user"
	AccessPublic Access = "public"
)

type ConnectionAuth string

const (
	ConnectionAuthOAuth ConnectionAuth = "oauth"
	ConnectionAuthToken ConnectionAuth = "token"
	ConnectionAuthNone  ConnectionAuth = "none"
)

type MCPAuth string

const (
	MCPAuthOAuth          MCPAuth = "oauth"
	MCPAuthOAuthDiscovery MCPAuth = "oauth_discovery"
	MCPAuthToken          MCPAuth = "token"
	MCPAuthNone           MCPAuth = "none"
)

type AuthInjectionType string

const (
	AuthInjectBearer     AuthInjectionType = "bearer"
	AuthInjectAPIKey     AuthInjectionType = "api_key_header"
	AuthInjectPathPrefix AuthInjectionType = "path_prefix"
	AuthInjectQueryParam AuthInjectionType = "query_param"
)

type AuthInjection struct {
	Type AuthInjectionType `json:"type"`
	Name string            `json:"name,omitempty"`
}

type DirectoryScope string

type ConnectionDef struct {
	Slug              string            `json:"slug,omitempty"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	BaseURL           string            `json:"baseUrl,omitempty"`
	AuthMode          ConnectionAuth    `json:"authMode"`
	AuthURL           string            `json:"authUrl,omitempty"`
	TokenURL          string            `json:"tokenUrl,omitempty"`
	Scopes            []string          `json:"scopes,omitempty"`
	AuthParams        map[string]string `json:"authParams,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	AuthInjection     AuthInjection     `json:"authInjection"`
	SetupInstructions string            `json:"setupInstructions,omitempty"`
	LLMHint           string            `json:"llmHint,omitempty"`
	Access            Access            `json:"access,omitempty"`
}

type ExecEndpointDef struct {
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description"`
	LLMHint     string `json:"llmHint,omitempty"`
	Access      Access `json:"access,omitempty"`
}

type Action struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMs int64     `json:"durationMs"`
	Request    any       `json:"request,omitempty"`
	Response   any       `json:"response,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type FileInfo struct {
	Path         string    `json:"path"`
	Filename     string    `json:"filename"`
	ContentType  string    `json:"contentType"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

type PromptInput struct {
	Messages         []message.Message `json:"messages"`
	Message          string            `json:"message,omitempty"`
	ConversationID   string            `json:"conversationId,omitempty"`
	ProviderID       string            `json:"providerId,omitempty"`
	ModelID          string            `json:"modelId,omitempty"`
	Temperature      *float64          `json:"temperature,omitempty"`
	MaxOutputTokens  *int              `json:"maxOutputTokens,omitempty"`
	ProviderOptions  json.RawMessage   `json:"providerOptions,omitempty"`
	Files            []FileInfo        `json:"files,omitempty"`
	ResumeRunID      string            `json:"resumeRunId,omitempty"`
	Approved         *bool             `json:"approved,omitempty"`
	Source           string            `json:"source,omitempty"`
	ExpectedSyncHash string            `json:"expectedSyncHash,omitempty"`
	Instructions     string            `json:"instructions,omitempty"`
	CallerAccess     Access            `json:"callerAccess,omitempty"`
	VisibleSiblings  []uuid.UUID       `json:"visibleSiblings,omitempty"`
	ForceCompact     bool              `json:"forceCompact,omitempty"`
	AutoConfirm      bool              `json:"autoConfirm,omitempty"`
	DirectTools      bool              `json:"directTools,omitempty"`
	Platform         string            `json:"platform,omitempty"`
	UserDisplayName  string            `json:"userDisplayName,omitempty"`
	UserEmail        string            `json:"userEmail,omitempty"`
}

type DirectoryDef struct {
	Path           string         `json:"path"`
	Read           Access         `json:"read"`
	Write          Access         `json:"write"`
	List           Access         `json:"list"`
	Description    string         `json:"description"`
	LLMHint        string         `json:"llmHint,omitempty"`
	RetentionHours int            `json:"retentionHours,omitempty"`
	Scope          DirectoryScope `json:"scope,omitempty"`
}

type TopicDef struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	LLMHint     string `json:"llmHint,omitempty"`
	Access      Access `json:"access"`
	PerUser     bool   `json:"perUser,omitempty"`
}

type DisplayPart struct {
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	Source   string  `json:"source,omitempty"`
	URL      string  `json:"url,omitempty"`
	Data     []byte  `json:"data,omitempty"`
	Filename string  `json:"filename,omitempty"`
	MimeType string  `json:"mimeType,omitempty"`
	Alt      string  `json:"alt,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

func ResolveDisplayPart(part *DisplayPart) {
	if len(part.Data) > 0 && part.MimeType == "" {
		part.MimeType = http.DetectContentType(part.Data)
	}
	if part.Type == "" {
		switch {
		case strings.HasPrefix(part.MimeType, "image/"):
			part.Type = "image"
		case strings.HasPrefix(part.MimeType, "audio/"):
			part.Type = "audio"
		case strings.HasPrefix(part.MimeType, "video/"):
			part.Type = "video"
		case part.MimeType != "":
			part.Type = "file"
		case part.Text != "":
			part.Type = "text"
		}
	}
	if part.Filename != "" || part.Type == "" || part.Type == "text" {
		return
	}
	switch {
	case part.Source != "":
		part.Filename = displayFilename(part.Source)
	case part.URL != "":
		part.Filename = displayFilename(part.URL)
	default:
		part.Filename = part.Type + displayExtension(part.MimeType, part.Type)
	}
}

func displayFilename(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return path
}

func displayExtension(mimeType, partType string) string {
	if extensions, _ := mime.ExtensionsByType(mimeType); len(extensions) > 0 {
		return extensions[0]
	}
	byMIME := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif",
		"image/webp": ".webp", "image/svg+xml": ".svg", "audio/mpeg": ".mp3",
		"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/ogg": ".ogg",
		"audio/webm": ".weba", "video/mp4": ".mp4", "video/webm": ".webm",
		"application/pdf": ".pdf", "application/json": ".json", "text/plain": ".txt",
		"text/csv": ".csv", "text/html": ".html",
	}
	if extension := byMIME[mimeType]; extension != "" {
		return extension
	}
	switch partType {
	case "image":
		return ".png"
	case "audio":
		return ".mp3"
	case "video":
		return ".mp4"
	default:
		return ".bin"
	}
}

type PrintRequest struct {
	Parts          []DisplayPart `json:"parts"`
	Topic          string        `json:"topic,omitempty"`
	ConversationID string        `json:"conversationId,omitempty"`
	RunID          string        `json:"runId,omitempty"`
	UserID         string        `json:"userId,omitempty"`
}

type MCPDef struct {
	Slug          string        `json:"slug,omitempty"`
	Name          string        `json:"name"`
	URL           string        `json:"url"`
	AuthMode      MCPAuth       `json:"authMode"`
	AuthURL       string        `json:"authUrl,omitempty"`
	TokenURL      string        `json:"tokenUrl,omitempty"`
	Scopes        []string      `json:"scopes,omitempty"`
	AuthInjection AuthInjection `json:"authInjection"`
	Access        Access        `json:"access,omitempty"`
}

type MCPToolSchema struct {
	ServerSlug  string          `json:"serverSlug"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type MCPAuthStatus struct {
	Slug         string  `json:"slug"`
	AuthMode     MCPAuth `json:"authMode"`
	Authorized   bool    `json:"authorized"`
	AuthURL      string  `json:"authUrl,omitempty"`
	Instructions string  `json:"instructions,omitempty"`
}

type MCPToolCallRequest struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type MCPToolCallResponse struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

type MCPContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// AgentManifest is the complete canonical declaration of an agent image. The
// SDK emits it in offline manifest mode and sends the same value during runtime
// synchronization. Slices are deterministically ordered by their identifiers,
// except Instructions and StartupHooks, whose registration order is semantic.
type AgentManifest struct {
	Version       string            `json:"version"`
	Description   string            `json:"description"`
	Emoji         string            `json:"emoji"`
	Tools         []ToolDef         `json:"tools"`
	Webhooks      []WebhookDef      `json:"webhooks"`
	JobHandlers   []JobHandlerDef   `json:"jobHandlers"`
	JobCrons      []JobCronDef      `json:"jobCrons"`
	Routes        []RouteDef        `json:"routes"`
	Topics        []TopicDef        `json:"topics"`
	MCPServers    []MCPDef          `json:"mcpServers"`
	Connections   []ConnectionDef   `json:"connections"`
	ExecEndpoints []ExecEndpointDef `json:"execEndpoints"`
	EnvVars       []EnvVarDef       `json:"envVars"`
	Directories   []DirectoryDef    `json:"directories"`
	Instructions  []InstructionDef  `json:"instructions"`
	ModelSlots    []ModelSlotDef    `json:"modelSlots"`
	StaticAssets  []StaticAssetDef  `json:"staticAssets"`
	StartupHooks  []StartupHookDef  `json:"startupHooks"`
}

// SyncRequest is the complete agent declaration accepted by runtime sync.
type SyncRequest = AgentManifest

type EnvVarDef struct {
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
	Default     string `json:"default,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}

type EnvVarValueResponse struct {
	Value string `json:"value"`
}

type ExecRequest struct {
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	StdinB64  string   `json:"stdinB64,omitempty"`
	TimeoutMs int64    `json:"timeoutMs,omitempty"`
}

type SealRequest struct {
	Plaintext string `json:"plaintext"`
}

type SealResponse struct {
	Sealed string `json:"sealed"`
}

type UnsealRequest struct {
	Sealed string `json:"sealed"`
}

type UnsealResponse struct {
	Plaintext string `json:"plaintext"`
}

type SessionLoadResponse struct {
	Messages []session.Message `json:"messages"`
	Revision string            `json:"revision"`
}

type SessionAppendRequest struct {
	Messages []session.Message `json:"messages"`
	Revision string            `json:"revision"`
}

type SessionAppendResponse struct {
	Revision string `json:"revision"`
}

type SessionCompactRequest struct {
	Summary     []session.Message `json:"summary"`
	TokensFreed int               `json:"tokensFreed"`
	Revision    string            `json:"revision"`
}

type SessionCompactResponse struct {
	Revision string `json:"revision"`
}

type InstructionDef struct {
	Text   string   `json:"text"`
	Access []Access `json:"access,omitempty"`
}

type ModelSlotDef struct {
	Slug        string `json:"slug"`
	Capability  string `json:"capability"`
	Description string `json:"description,omitempty"`
}

type StaticAssetDef struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

type StartupHookDef struct {
	Name string `json:"name"`
}

type SyncResponse struct {
	PromptData        PromptData                 `json:"promptData"`
	MCPAuthStatus     []MCPAuthStatus            `json:"mcpAuthStatus,omitempty"`
	MCPSchemas        map[string][]MCPToolSchema `json:"mcpSchemas,omitempty"`
	PublicStorageBase string                     `json:"publicStorageBase,omitempty"`
	SyncStateHash     string                     `json:"syncStateHash,omitempty"`
}

type PromptData struct {
	AgentDashboardURL   string        `json:"agentDashboardUrl"`
	AgentRouteURL       string        `json:"agentRouteUrl"`
	Siblings            []SiblingInfo `json:"siblings,omitempty"`
	Capabilities        Capabilities  `json:"capabilities,omitempty"`
	SupportedModalities []string      `json:"supportedModalities,omitempty"`
}

type Capabilities struct {
	Vision        bool `json:"vision,omitempty"`
	Transcription bool `json:"transcription,omitempty"`
	Speech        bool `json:"speech,omitempty"`
	Embedding     bool `json:"embedding,omitempty"`
	Image         bool `json:"image,omitempty"`
	Search        bool `json:"search,omitempty"`
}

type SiblingInfo struct {
	ID          uuid.UUID       `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Tools       []MCPToolSchema `json:"tools,omitempty"`
}

type ToolDef struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	LLMHint       string            `json:"llmHint,omitempty"`
	Access        Access            `json:"access"`
	InputSchema   json.RawMessage   `json:"inputSchema,omitempty"`
	OutputSchema  json.RawMessage   `json:"outputSchema,omitempty"`
	InputExamples []json.RawMessage `json:"inputExamples,omitempty"`
}

type RouteDef struct {
	Path        string `json:"path"`
	Method      string `json:"method"`
	Access      Access `json:"access"`
	Description string `json:"description,omitempty"`
}

type WebhookDef struct {
	Path        string `json:"path"`
	Verify      string `json:"verify"`
	Header      string `json:"header,omitempty"`
	TimeoutMs   int64  `json:"timeoutMs"`
	Description string `json:"description,omitempty"`
}

type JobHandlerDef struct {
	Name             string          `json:"name"`
	Version          int32           `json:"version"`
	Description      string          `json:"description"`
	TimeoutMs        int64           `json:"timeoutMs"`
	MaxAttempts      int32           `json:"maxAttempts"`
	MaxConcurrency   int32           `json:"maxConcurrency"`
	InputSchema      json.RawMessage `json:"inputSchema"`
	OutputSchema     json.RawMessage `json:"outputSchema"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
}

type JobCronDef struct {
	Slug             string          `json:"slug"`
	Schedule         string          `json:"schedule"`
	Description      string          `json:"description"`
	HandlerName      string          `json:"handlerName"`
	HandlerVersion   int32           `json:"handlerVersion"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
	Input            json.RawMessage `json:"input"`
}

// JobManifest is the canonical job declaration emitted by an agent image in
// job-manifest inspection mode. Both slices are ordered by their identifiers.
type JobManifest struct {
	JobHandlers []JobHandlerDef `json:"jobHandlers"`
	JobCrons    []JobCronDef    `json:"jobCrons"`
}

type JobRunRequest struct {
	ID                      string          `json:"id"`
	Name                    string          `json:"name"`
	Version                 int32           `json:"version"`
	InputSchemaHash         string          `json:"inputSchemaHash"`
	OutputSchemaHash        string          `json:"outputSchemaHash"`
	Attempt                 int32           `json:"attempt"`
	TimeoutMs               int64           `json:"timeoutMs"`
	Input                   json.RawMessage `json:"input"`
	ScheduledAt             *time.Time      `json:"scheduledAt,omitempty"`
	InitiatorKind           string          `json:"initiatorKind"`
	InitiatorUserID         string          `json:"initiatorUserId"`
	InitiatorConversationID string          `json:"initiatorConversationId"`
	CallerAccess            Access          `json:"callerAccess"`
}

type JobRunResponse struct {
	// Status is success, error, timeout, or retry. Retry reports a temporary
	// enqueue availability failure and asks Airlock to redeliver the attempt.
	Status string          `json:"status"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type EnqueueJobRequest struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Version          int32           `json:"version"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
	Input            json.RawMessage `json:"input"`
	ScheduledAt      *time.Time      `json:"scheduledAt,omitempty"`
}

type JobProgress struct {
	Phase     string `json:"phase"`
	Message   string `json:"message"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
}

type UpdateJobProgressRequest struct {
	Attempt   int32  `json:"attempt"`
	Phase     string `json:"phase"`
	Message   string `json:"message"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
}

type JobInfo struct {
	ID               string          `json:"id"`
	AgentID          string          `json:"agentId"`
	HandlerName      string          `json:"handlerName"`
	HandlerVersion   int32           `json:"handlerVersion"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
	Status           string          `json:"status"`
	Input            json.RawMessage `json:"input"`
	Output           json.RawMessage `json:"output,omitempty"`
	AttemptCount     int32           `json:"attemptCount"`
	MaxAttempts      int32           `json:"maxAttempts"`
	AttemptLimit     int32           `json:"attemptLimit"`
	LastError        string          `json:"lastError,omitempty"`
	Progress         *JobProgress    `json:"progress,omitempty"`
	SourceRunID      string          `json:"sourceRunId,omitempty"`
	ScheduledAt      *time.Time      `json:"scheduledAt,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	CompletedAt      *time.Time      `json:"completedAt,omitempty"`
}

type EnqueueJobResponse struct {
	Job     JobInfo `json:"job"`
	Created bool    `json:"created"`
}

const EnqueueJobErrorCodeUnavailable = "enqueue_unavailable"

// EnqueueJobErrorResponse is returned with HTTP 409 when an exact job handler
// contract is temporarily unavailable during a deployment transition.
type EnqueueJobErrorResponse struct {
	Code           string `json:"code"`
	Error          string `json:"error"`
	HandlerName    string `json:"handlerName"`
	HandlerVersion int32  `json:"handlerVersion"`
}

type GetJobResponse struct {
	Job JobInfo `json:"job"`
}

type ListJobsResponse struct {
	Jobs       []JobInfo `json:"jobs"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type HTTPRequest struct {
	URL        string            `json:"url"`
	Method     string            `json:"method,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
	SaveAs     string            `json:"saveAs,omitempty"`
	RunID      string            `json:"runId,omitempty"`
	Raw        bool              `json:"raw,omitempty"`
	AllHeaders bool              `json:"allHeaders,omitempty"`
}

type HTTPResponse struct {
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"contentType"`
	Size        int               `json:"size"`
	BodyPreview string            `json:"bodyPreview,omitempty"`
	SavedTo     string            `json:"savedTo,omitempty"`
	Note        string            `json:"note,omitempty"`
}

type ProxyRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type ShareFileRequest struct {
	Path           string `json:"path"`
	ExpiresSeconds int64  `json:"expiresSeconds,omitempty"`
}

type ShareFileResponse struct {
	URL         string `json:"url"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

type LLMProxyRequest struct {
	Slug       string          `json:"slug,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Options    json.RawMessage `json:"options"`
}

type ModelProxyRequest struct {
	Slug       string          `json:"slug,omitempty"`
	Capability string          `json:"capability"`
	Options    json.RawMessage `json:"options"`
}

type CreateRunRequest struct {
	TriggerType    string `json:"triggerType"`
	TriggerRef     string `json:"triggerRef"`
	UserID         string `json:"userId,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	CallerAccess   Access `json:"callerAccess"`
}

type CreateRunResponse struct {
	RunID string `json:"runId"`
}

type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

type LogEntry struct {
	Level   LogLevel `json:"level"`
	Message string   `json:"message"`
}

const (
	ErrorKindPlatform = "platform"
	ErrorKindAgent    = "agent"
)

type RunCompleteRequest struct {
	RunID      string          `json:"runId"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	ErrorKind  string          `json:"errorKind,omitempty"`
	PanicTrace string          `json:"panicTrace,omitempty"`
	Actions    []Action        `json:"actions"`
	Logs       []LogEntry      `json:"logs,omitempty"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
}

type SearchProxyRequest struct {
	Slug       string `json:"slug,omitempty"`
	Capability string `json:"capability,omitempty"`
	websearch.Request
}
