package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout is the default execution timeout for webhooks and crons.
const defaultTimeout = 2 * time.Minute

// User identifies the human a run is acting for, exposed to handler code via
// UserFromContext and to run_js as the `user` global. ID is the stable
// internal-user uuid (the key to scope agent-owned data by); Email/DisplayName
// are display claims. All fields are empty for cron/schedule/webhook and
// anonymous runs.
type User struct {
	ID          string
	Email       string
	DisplayName string
}

// --- Handler types ---

// WebhookHandlerFunc handles incoming webhook requests. Pass ctx to any
// agent.X(ctx, ...) call the body makes.
type WebhookHandlerFunc func(ctx context.Context, data []byte, ew *EventWriter) error

// ScheduleHandlerFunc handles a timed fire of a registered cron or schedule.
// It carries no payload — per-instance data lives in the agent's own DB,
// keyed by the fire id (see ScheduleFromContext).
type ScheduleHandlerFunc func(ctx context.Context, ew *EventWriter) error

// RouteHandlerFunc handles custom HTTP routes registered via RegisterRoute.
// Handler code uses r.Context() for agent calls and returns execution errors to
// the SDK. Errors are recorded on a materialized run and served as a generic
// 500 response when the handler has not already written a response.
type RouteHandlerFunc func(w http.ResponseWriter, r *http.Request) error

// --- Webhook ---

// Webhook is the self-contained declaration registered via agent.RegisterWebhook.
// Agents serve incoming HTTP at /webhook/{Path} on their container.
type Webhook struct {
	Path        string             // unique per agent
	Handler     WebhookHandlerFunc // required
	Verify      string             // required: "none" | "hmac" | "token" | "bearer" | "ed25519"
	Header      string             // signature header (required for hmac, optional for ed25519)
	Timeout     time.Duration      // max execution time (default: 2 min)
	Description string             // required: shown to users and the LLM
}

// --- Cron ---

// Cron is a recurring, code-declared schedule registered via agent.RegisterCron.
// It fires by schedule, never by user action — no Access field. The slug shares
// one namespace with RegisterSchedule (unique per agent).
type Cron struct {
	Slug        string              // unique per agent (across crons + schedules)
	Schedule    string              // standard cron expression, e.g. "0 9 * * *"
	Handler     ScheduleHandlerFunc // required
	Timeout     time.Duration       // max execution time (default: 2 min)
	Description string              // required: shown to users
}

// Schedule is a code-declared handler for runtime-armed one-shot fires
// registered via agent.RegisterSchedule. Arm an instance with agent.ScheduleAt;
// per-instance data lives in the agent's own DB (keyed by the returned fire id),
// not in the platform. No Access field — fires are trusted/system.
type Schedule struct {
	Slug        string              // unique per agent (across crons + schedules)
	Handler     ScheduleHandlerFunc // required
	Timeout     time.Duration       // max execution time (default: 2 min)
	Description string              // required: shown to users
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
	Description string           // required: shown to users and the LLM
}

// --- Connection ---

// Connection is the self-contained declaration registered via
// agent.RegisterConnection — an outgoing service Airlock proxies for the agent
// with credentials it manages.
type Connection struct {
	Slug        string         // unique per agent; binds as conn_{slug} in run_js
	Name        string         // required
	Description string         // required: shown to users and the LLM
	BaseURL     string         // required: absolute HTTP(S) URL
	AuthMode    ConnectionAuth // required
	AuthURL     string
	TokenURL    string
	Scopes      []string
	// AuthParams are extra query parameters added to the OAuth
	// authorization request, overriding the platform defaults per key.
	// Optional escape hatch for providers whose refresh-token handshake
	// differs from the default.
	AuthParams map[string]string
	// Headers are static request headers Airlock sets on every proxied
	// call for this connection (User-Agent, Accept, X-Foo, …). Merged
	// per-key on top of the platform baseline (a real-browser UA); the
	// caller's per-call RequestOpts.Headers merge on top in turn. Set a
	// value to the empty string to drop a baseline key entirely.
	Headers           map[string]string
	AuthInjection     AuthInjection
	SetupInstructions string
	LLMHint           string // appended to the connection block in the system prompt
	Access            Access // required: who may invoke conn_{slug}
}

// ConnectionResponse is the streaming primitive returned by
// ConnectionHandle.RequestStream. Body is the upstream response body,
// streamed through airlock's proxy with no airlock-side buffering. Caller
// owns the lifetime — defer Body.Close() once you've finished reading.
//
// StatusCode and Headers carry the upstream values verbatim; airlock
// removes only its own auth-injection headers. A 2xx from upstream comes
// through as a 2xx here; auth-required surfaces as *AuthRequiredError on
// the parent Request* call (not via this struct).
type ConnectionResponse struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
}

// RequestOpts is the call shape for ConnectionHandle.Request /
// RequestStream / RequestJSON. Mirrors the options-dict pattern of
// axios / fetch / python-requests so call sites read declaratively
// instead of positionally — most calls only need Path, and adding
// Body or Headers later is a structural edit instead of a shift of
// every argument.
//
//	// Simple GET (Method defaults to "GET"):
//	body, _ := conn.Request(ctx, agentsdk.RequestOpts{Path: "/v1/me"})
//
//	// POST with body:
//	conn.Request(ctx, agentsdk.RequestOpts{
//	    Method: "POST", Path: "/v1/playlists", Body: playlist,
//	})
//
//	// With per-call headers:
//	conn.Request(ctx, agentsdk.RequestOpts{
//	    Path:    "/v1/me/player",
//	    Headers: map[string]string{"If-None-Match": etag},
//	})
type RequestOpts struct {
	// Method is the HTTP verb. Empty defaults to "GET" (the majority
	// of calls).
	Method string
	// Path is appended to the connection's BaseURL. Required.
	Path string
	// Body is encoded by type when non-nil: []byte / string sent as-is,
	// io.Reader fully read, anything else JSON-marshalled.
	Body any
	// Headers merge per-key on top of the platform baseline (real-browser
	// User-Agent) and the connection's declared Headers. Set a value to
	// the empty string to suppress a key set by a lower layer. Nil/empty
	// map means no overrides.
	Headers map[string]string
}

// AuthInjection defines how auth credentials are injected into proxied requests.
// Name carries the header or query-parameter name depending on Type:
//   - api_key_header: header name (default "X-API-Key")
//   - query_param:    query-string key (default "token")
//   - bearer / path_prefix: ignored
type AuthInjection struct {
	Type AuthInjectionType `json:"type"`
	Name string            `json:"name,omitempty"`
}

// AuthInjectionType selects how the proxy injects the stored credential into
// each upstream request.
type AuthInjectionType string

const (
	// AuthInjectBearer sets `Authorization: Bearer {token}`.
	AuthInjectBearer AuthInjectionType = "bearer"
	// AuthInjectAPIKey sets a custom header `{Name}: {token}` (Name defaults
	// to "X-API-Key").
	AuthInjectAPIKey AuthInjectionType = "api_key_header"
	// AuthInjectPathPrefix prepends `/{token}` to the URL path. Used by
	// APIs that carry credentials in the path (e.g. Telegram bot API).
	AuthInjectPathPrefix AuthInjectionType = "path_prefix"
	// AuthInjectQueryParam appends `?{Name}={token}` (or merges into existing
	// query string). Name defaults to "token". Used by MCP servers and APIs
	// that auth via URL query strings.
	AuthInjectQueryParam AuthInjectionType = "query_param"
)

// --- Exec endpoints ---

// ExecEndpoint is the self-contained declaration registered via
// agent.RegisterExecEndpoint — a remote target airlock executes commands
// against on the agent's behalf. The transport (ssh today; telnet,
// endpoint-binary later) and credentials are operator-configured via the
// Airlock UI; the agent's main() only declares slug + description + access.
type ExecEndpoint struct {
	Slug        string // unique per agent; binds as exec_{slug} in run_js
	Description string
	LLMHint     string // appended to the endpoint block in the system prompt
	Access      Access // required: AccessAdmin or AccessUser
}

// ExecCommand is the input to ExecHandle.Run / ExecHandle.RunStream.
//
// Command is handed to the remote shell as a single command line: pipes,
// redirection, and shell substitution in Command just work because the
// remote sshd execs the user's login shell with it. Args are
// POSIX-shell-quoted and space-joined onto Command before send, so
// Run("ls", []string{"-la", "my dir"}) sends `ls -la 'my dir'` safely.
//
// Use Args for safe multi-arg commands; put any shell features (pipes,
// redirection) in Command and leave Args empty.
type ExecCommand struct {
	Command string        `json:"command"`
	Args    []string      `json:"args,omitempty"`
	Stdin   []byte        `json:"-"` // marshalled separately as base64
	Timeout time.Duration `json:"-"` // 0 = server default (60s); marshalled as timeoutMs
}

// ExecResult is what Run returns when the call fits in the 20 MiB buffer
// cap. Overflow returns ErrOutputTooLarge with no partial result — use
// RunStream for outputs that may exceed the cap.
type ExecResult struct {
	Stdout     []byte
	Stderr     []byte
	ExitCode   int
	DurationMs int64
}

// ExecExit is the terminal status of a streaming exec call. Returned by
// ExecStream.Wait once the remote has closed both stdout and stderr.
type ExecExit struct {
	ExitCode   int
	DurationMs int64
}

// ExecStream is the streaming primitive returned by ExecHandle.RunStream.
// Mirrors os/exec.Cmd's StdoutPipe / StderrPipe / Wait shape so Go users
// get a familiar mental model:
//
//	s, _ := vps.RunStream(ctx, ExecCommand{Command: "tar -czf - /var/log"})
//	defer s.Stdout.Close()
//	defer s.Stderr.Close()
//	info, _ := agent.WriteFile(ctx, "tmp/logs.tar.gz", s.Stdout, "application/gzip")
//	exit, _ := s.Wait()
//
// Stdout and Stderr stay open until the remote closes its side; Wait
// blocks until the exit envelope arrives. Always close both pipes — even
// when you only care about one — to release the demux goroutines.
type ExecStream struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Wait   func() (ExecExit, error)
}

// ExecError distinguishes transport-class problems (the command never
// ran) from runtime failures (the command ran and reported a non-zero
// exit code, which is just an ExecResult with a non-zero ExitCode).
type ExecError struct {
	Kind    string // "transport" | "timeout" | "config" | "denied"
	Message string
}

func (e *ExecError) Error() string { return "exec " + e.Kind + ": " + e.Message }

// ErrOutputTooLarge is returned by Run / Request when the response exceeds
// the 20 MiB buffered cap. The error message points the caller at the
// streaming variant as the resolution.
var ErrOutputTooLarge = errors.New("agentsdk: response exceeded 20 MiB buffer cap; Run/Request are for structured small responses (JSON, HTML, CLI summaries) — use RunStream/RequestStream for any data download")

// --- Files ---

// FileInfo describes a file in agent storage. Returned by StatFile, ListDir,
// WriteFile, and embedded in promptInput.Files for chat uploads. Path is the
// canonical identifier; Filename is the original upload name preserved as S3
// metadata so the LLM can refer to "Q1 Report.pdf" while the path uses a
// uuid-prefixed safe filename.
type FileInfo struct {
	Path         FilePath  `json:"path"`     // S3-style storage path, e.g. "uploads/foo.png"
	Filename     string    `json:"filename"` // original upload name; S3 metadata
	ContentType  string    `json:"contentType"`
	Size         int64     `json:"size"`
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

// --- Directories ---

// Directory is the self-contained declaration registered via
// agent.RegisterDirectory. Each directory owns an S3 prefix
// ("agents/{agentID}/{Path}") and gates access through three independent
// caps.
//
// The framework auto-registers a reserved directory "tmp" at
// Read=Write=List=AccessUser; builder calls with Path="tmp" silently
// keep the framework's caps (Description may still be supplied).
//
// Read, Write, and List are independent. delete folds into Write (write
// on the parent governs unlink), so DeleteFile requires Write access.
type Directory struct {
	Path        string // S3-style path with no leading '/', e.g. "reports"; no '..' or '//'; no trailing slash
	Read        Access // gates ReadFile / OpenFile / StatFile + the public read route
	Write       Access // gates WriteFile / DeleteFile + the public write route
	List        Access // gates ListDir
	Description string // shown in the system prompt's directories section

	// LLMHint is optional guidance shown to the LLM in the system prompt
	// alongside the directory entry, e.g. "internal cache; avoid listing
	// or modifying" or "user-uploaded reports; prefer summarizing over
	// quoting". Authorization stays with Read/Write/List — LLMHint only
	// steers the model. Empty by default.
	LLMHint string

	// RetentionHours, when > 0, opts the directory into Airlock's storage
	// sweeper: any file in the S3 prefix older than this many hours is
	// deleted on the next sweep tick (~6h cadence). Zero means files
	// stay forever — that's the default for normal builder directories.
	// The framework's /tmp registers with 72 to garbage-collect chat
	// uploads and generated media; tools that produce throwaway artifacts
	// (e.g. AI-generated images served via fileShareURL with a 1h URL
	// expiry) should set a matching short TTL so the bytes go away when
	// the URL does.
	RetentionHours int

	// Scope opts the directory into per-context isolation: WriteFile
	// transparently inserts a scope segment (user-<id>/conv-<id>/run-<id>)
	// between the directory prefix and the rest of the path, and reads
	// only succeed when the scope key in the path matches one the
	// current run owns. Use it for directories accessible to lower-trust
	// callers (public-MCP, anon) where you need per-caller isolation
	// without sacrificing usability — the LLM sees the scoped path,
	// passes it around, and access just works for the caller who wrote
	// it. Default ScopeNone preserves today's behaviour.
	Scope DirectoryScope
}

// DirectoryOpts is the option struct accepted by RegisterDirectory.
type DirectoryOpts struct {
	Read        Access // required
	Write       Access // required
	List        Access // required
	Description string // required: shown to the LLM

	// LLMHint: see Directory.LLMHint. Optional model-facing guidance.
	LLMHint string

	// RetentionHours: see Directory.RetentionHours. Zero = no sweep.
	RetentionHours int

	// Scope: see Directory.Scope. Default ScopeNone (no scoping).
	Scope DirectoryScope
}

// DirectoryScope opts a directory into per-context path scoping. See
// Directory.Scope. Empty string ("" / ScopeNone) keeps the legacy
// unscoped behaviour: base ACL is the only access gate.
//
// The three values map to the three identities a run is naturally
// anchored against: the calling user, the current conversation, and
// this single call. WriteFile picks the strongest available key from
// the run when scoping a path (user → conv → run); CheckFileAccess
// accepts any of the three on read, so a path written at user-scope
// remains readable from any run serving the same user.
type DirectoryScope string

const (
	ScopeNone DirectoryScope = ""
	ScopeRun  DirectoryScope = "run"
	ScopeConv DirectoryScope = "conv"
	ScopeUser DirectoryScope = "user"
)

// FileOp tags an operation passed to CheckFileAccess. Delete folds into
// OpWrite (write on the parent governs unlink); there is no separate
// OpDelete.
type FileOp string

const (
	OpRead  FileOp = "read"
	OpWrite FileOp = "write"
	OpList  FileOp = "list"
)

// --- Topic ---

// Topic is the self-contained declaration registered via agent.RegisterTopic.
// Conversations subscribe to a topic via topic_{slug}.subscribe() in run_js;
// builders publish via the *TopicHandle returned by RegisterTopic.
type Topic struct {
	Slug        string
	Description string
	LLMHint     string // optional model-only guidance — see Directory.LLMHint
	Access      Access // required: who may subscribe via topic_{slug}.subscribe()
	// PerUser forbids broadcast: Publish panics, only PublishToUser delivers
	// (to the named user's subscribed conversations). Use for personal feeds
	// (reminders, alerts) where a broadcast would leak across users.
	PerUser bool
}

// --- Display parts (output / topic publish) ---

// DisplayPart is a single piece of rich content for user-facing output.
// The `output` JS binding accepts media-only parts
// (image/file/audio/video); TopicHandle.Publish accepts text too,
// since Go builder code has no separate prose channel to use instead.
type DisplayPart struct {
	Type     string  `json:"type"`             // "text", "image", "file", "audio", "video"
	Text     string  `json:"text,omitempty"`   // body text, or caption for media types
	Source   string  `json:"source,omitempty"` // S3 key
	URL      string  `json:"url,omitempty"`    // external URL
	Data     []byte  `json:"data,omitempty"`   // raw bytes (base64 in JSON)
	Filename string  `json:"filename,omitempty"`
	MimeType string  `json:"mimeType,omitempty"`
	Alt      string  `json:"alt,omitempty"`      // accessibility text for images
	Duration float64 `json:"duration,omitempty"` // seconds, audio/video
}

// resolveDisplayPart infers missing fields on a DisplayPart from available data.
// 1. If Data is set but MimeType is empty → detect from bytes.
// 2. If MimeType is set but Type is empty → infer from MIME prefix.
// 3. If Filename is empty and part has media → generate from type + mimeType.
func resolveDisplayPart(p *DisplayPart) {
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

	// Generate filename for media parts without one. Priority:
	//   1. Source path's basename — preserves the real filename the
	//      caller picked ("red-square-16x16.png" stays a .png file
	//      end-to-end, including in the presigned-URL tail clients
	//      read to choose a Save-As name). Before this, the type+ext
	//      generator would overwrite a Source-based part with
	//      "image.png" / "image.bin", losing the original name.
	//   2. URL's last path segment — same reasoning for external URLs.
	//   3. Type + extension from MimeType — only when neither Source
	//      nor URL gives us a real filename. mime.ExtensionsByType is
	//      OS-dependent (reads /etc/mime.types), so we fall back to a
	//      baked-in map (extForMimeOrType) so a missing mime DB doesn't
	//      give every file ".bin".
	if p.Filename == "" && p.Type != "" && p.Type != "text" {
		switch {
		case p.Source != "":
			p.Filename = filenameFromPath(p.Source)
		case p.URL != "":
			p.Filename = filenameFromPath(p.URL)
		default:
			p.Filename = p.Type + extForMimeOrType(p.MimeType, p.Type)
		}
	}
}

// filenameFromPath returns the last slash-segment of a path / URL,
// stripped of any leading query string. Returns "" when the input has
// no usable tail (caller falls through to a synthesized filename).
func filenameFromPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	for strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// extForMimeOrType returns a leading-dot extension for the given mime
// type, falling back to the part type when mime is empty/unknown. The
// baked-in map is small and covers what http.DetectContentType actually
// emits — it doesn't try to be exhaustive.
func extForMimeOrType(mimeType, partType string) string {
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return exts[0]
	}
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/webm":
		return ".weba"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "application/pdf":
		return ".pdf"
	case "application/json":
		return ".json"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "text/html":
		return ".html"
	}
	// Type-only fallback when mime is empty: pick a sensible default
	// for each media category. Better than .bin in the common case
	// where the agent passed bytes with no mime hint.
	switch partType {
	case "image":
		return ".png"
	case "audio":
		return ".mp3"
	case "video":
		return ".mp4"
	}
	return ".bin"
}

// --- Access levels ---

// Access defines who can reach a tool, connection, MCP, topic, or storage zone.
type Access string

const (
	AccessAdmin  Access = "admin"
	AccessUser   Access = "user"
	AccessPublic Access = "public"
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
	Slug     string  // unique per agent; binds as mcp_{slug} in run_js
	Name     string  // required
	URL      string  // required: absolute HTTP(S) URL
	AuthMode MCPAuth // required
	AuthURL  string
	TokenURL string
	Scopes   []string
	// AuthInjection picks how the stored credential is added to each MCP
	// HTTP call: bearer header (default), custom header, query parameter,
	// or path prefix. Mirrors Connection.AuthInjection.
	AuthInjection AuthInjection
	Access        Access // required: who may invoke mcp_{slug}
}

// MCPToolCallResponse is returned from MCP tool call proxy.
type MCPToolCallResponse struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError"`
}

// MCPContent is a single content block in an MCP tool response.
// MCP defines five content types; we keep the fields we surface to JS
// callers. URI is set for resource_link; Data + MimeType for
// image/audio; Name for resource_link display.
type MCPContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// Instruction is the self-contained declaration passed to agent.AddInstruction.
// The Text fragment is appended to the system prompt for runs whose caller
// access matches one of the listed Access levels. Empty Access slice means
// "applies to every access level."
type Instruction struct {
	Text   string
	Access []Access
}

// ModelSlot is the self-contained declaration registered via agent.RegisterModel.
type ModelSlot struct {
	Slug        string
	Capability  ModelCapability // required: CapText, CapVision, CapImage, CapSpeech, CapTranscription, CapEmbedding, or CapSearch
	Description string          // required: human-readable hint shown in the admin UI
}

// ScheduleAtRequest arms a one-shot fire of a registered handler. Body of
// POST /api/agent/schedules; the response carries the new fire id.
type ScheduleAtRequest struct {
	Slug   string    `json:"slug"`
	FireAt time.Time `json:"fireAt"`
}

// ScheduledFire is one pending/recorded fire row, returned by ListSchedules.
type ScheduledFire struct {
	ID         string    `json:"id"`
	Slug       string    `json:"slug"`
	Kind       string    `json:"kind"` // "cron" | "schedule"
	FireAt     time.Time `json:"fireAt"`
	Status     string    `json:"status"` // pending|fired|error|orphaned|cancelled
	Recurrence string    `json:"recurrence,omitempty"`
}

// ScheduledFireRef identifies the fire that triggered the current handler run.
// Read it with ScheduleFromContext to look up the per-instance data the agent
// stored in its own DB at ScheduleAt time.
type ScheduledFireRef struct {
	FireID string
	Slug   string
}

// ShareFileResponse is returned by POST /api/agent/storage/share.
// URL is unauthenticated and valid until ExpiresAtMs (ms epoch).
type ShareFileResponse struct {
	URL         string `json:"url"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
}

// --- Model capability types ---

// ModelCapability describes what kind of model is needed.
type ModelCapability string

const (
	CapText          ModelCapability = "text"          // any chat/language model
	CapVision        ModelCapability = "vision"        // chat model that accepts images
	CapEmbedding     ModelCapability = "embedding"     // vector embeddings
	CapImage         ModelCapability = "image"         // image generation
	CapSpeech        ModelCapability = "speech"        // text-to-speech
	CapTranscription ModelCapability = "transcription" // speech-to-text
	CapSearch        ModelCapability = "search"        // web search provider (provider-bound, optional model)
)
