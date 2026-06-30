package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
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
type RouteHandlerFunc func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// --- Webhook ---

// Webhook is the self-contained declaration registered via agent.RegisterWebhook.
// Agents serve incoming HTTP at /webhook/{Path} on their container.
type Webhook struct {
	Path        string             // unique per agent
	Handler     WebhookHandlerFunc // required
	Verify      string             // "none" | "hmac" | "token" | "bearer" | "ed25519" (default: "none")
	Header      string             // header carrying the signature/token (hmac/ed25519 modes)
	Timeout     time.Duration      // max execution time (default: 2 min)
	Description string
	Access      Access // who may invoke; default AccessUser
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
	Description string
}

// Schedule is a code-declared handler for runtime-armed one-shot fires
// registered via agent.RegisterSchedule. Arm an instance with agent.ScheduleAt;
// per-instance data lives in the agent's own DB (keyed by the returned fire id),
// not in the platform. No Access field — fires are trusted/system.
type Schedule struct {
	Slug        string              // unique per agent (across crons + schedules)
	Handler     ScheduleHandlerFunc // required
	Timeout     time.Duration       // max execution time (default: 2 min)
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
	Slug        string // unique per agent; binds as conn_{slug} in run_js
	Name        string
	Description string
	BaseURL     string
	AuthMode    ConnectionAuth
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
	// caller's per-call ProxyRequest.Headers merge on top in turn. Set a
	// value to the empty string to drop a baseline key entirely.
	Headers           map[string]string
	AuthInjection     AuthInjection
	SetupInstructions string
	LLMHint           string // appended to the connection block in the system prompt
	Access            Access // who may invoke conn_{slug}; default AccessUser
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

// ConnectionDef is the wire format used by PUT /api/agent/connections/{slug}.
// Slug is sent in the URL, not the body.
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
	Access      Access // who may invoke; default AccessAdmin; AccessPublic is silently demoted to AccessUser
}

// ExecEndpointDef is the wire format used by PUT /api/agent/exec-endpoints/{slug}.
// Slug travels in the URL. Operator-configured fields stay airlock-side and
// are not present here — the agent only declares its intent to use the slug.
type ExecEndpointDef struct {
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description"`
	LLMHint     string `json:"llmHint,omitempty"`
	Access      Access `json:"access,omitempty"`
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

// --- Run recording ---

// Action records a single operation performed during a Run.
type Action struct {
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMs int64     `json:"durationMs"`
	Request    any       `json:"request,omitempty"`
	Response   any       `json:"response,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// --- Files ---

// FileInfo describes a file in agent storage. Returned by StatFile, ListDir,
// WriteFile, and embedded in PromptInput.Files for chat uploads. Path is the
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

// --- Prompt input ---

// PromptInput is the request body for POST /prompt.
type PromptInput struct {
	Messages            []message.Message `json:"messages"`
	Message             string            `json:"message,omitempty"` // New user message text (used with SessionStore)
	ConversationID      string            `json:"conversationId,omitempty"`
	ProviderID          string            `json:"providerId,omitempty"`
	ModelID             string            `json:"modelId,omitempty"`
	Temperature         *float64          `json:"temperature,omitempty"`
	MaxOutputTokens     *int              `json:"maxOutputTokens,omitempty"`
	ProviderOptions     json.RawMessage   `json:"providerOptions,omitempty"`
	Files               []FileInfo        `json:"files,omitempty"`
	ResumeRunID         string            `json:"resumeRunId,omitempty"`
	Approved            *bool             `json:"approved,omitempty"`
	SupportedModalities []string          `json:"supportedModalities,omitempty"` // e.g. ["text", "image", "pdf", "audio", "video"]
	Source              string            `json:"source,omitempty"`              // "user" (default), "system" (injected by Airlock)

	// Instructions is an access-filtered concatenation of the agent's
	// registered AddInstruction fragments, composed by Airlock at run
	// dispatch. The agent appends this to its sync-cached system prompt.
	Instructions string `json:"instructions,omitempty"`

	// CallerAccess is the resolved per-(agent, user) access level for the
	// triggering caller. agentsdk uses it to gate which conn_/mcp_/topic_/
	// storage_ JS bindings (and registered tools) are exposed to the run.
	// Airlock sets this from trigger.ResolveAgentAccess. For trusted server
	// triggers (webhooks, crons) Airlock sends AccessAdmin.
	CallerAccess Access `json:"callerAccess,omitempty"`

	// VisibleSiblings are the sibling-agent IDs this run's user is
	// authorized to A2A-call. UUIDs (not slugs) so a mid-run rename
	// doesn't silently revoke or reassign bindings. Computed by Airlock
	// at dispatch using the same access ladder that gates the MCP
	// endpoint. agentsdk intersects this with the sync-cached
	// PromptData.Siblings (matched on .ID) for both prompt rendering
	// and VM bindings — so the prompt and the runtime agree about which
	// agent_<slug> namespaces are reachable on this run.
	VisibleSiblings []uuid.UUID `json:"visibleSiblings,omitempty"`

	// ForceCompact tells the agent to skip the thinking loop and run a
	// user-triggered compaction instead. Message is ignored when set. The
	// agent loads conversation history, asks the model to summarize it,
	// persists the summary via the SessionStore's Compact method, and emits
	// a short text-delta describing the outcome.
	ForceCompact bool `json:"forceCompact,omitempty"`

	// AutoConfirm makes run_js skip the request_confirmation gate and
	// execute directly. Airlock sets it for runs that have no interactive
	// second turn in which to answer a confirmation — currently public
	// one-shot bridge sessions. It governs only this run's own run_js; a
	// suspension that still reaches the triggering surface by another path
	// (e.g. an A2A-delegated confirmation) is auto-denied there instead.
	AutoConfirm bool `json:"autoConfirm,omitempty"`

	// DirectTools selects the per-run tool surface. When false (default),
	// the LLM gets one `run_js` tool and capabilities are JS bindings inside
	// the goja sandbox. When true, every capability is its own typed LLM
	// tool — no JS sandbox, no TypeScript manifest in the prompt. Airlock
	// sets this based on the resolved caller; today it's hardcoded to
	// `callerAccess == AccessPublic`.
	DirectTools bool `json:"directTools,omitempty"`

	// Platform / UserDisplayName / UserEmail are per-turn context for the
	// prompt's <env> block. Airlock sets Platform explicitly per dispatch
	// path (web/telegram/discord/a2a — never inferred) and resolves the
	// originating user's name/email; any may be empty (then omitted).
	Platform        string `json:"platform,omitempty"`
	UserDisplayName string `json:"userDisplayName,omitempty"`
	UserEmail       string `json:"userEmail,omitempty"`
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
	Read        Access // default AccessUser
	Write       Access // default AccessUser
	List        Access // default AccessUser
	Description string

	// LLMHint: see Directory.LLMHint. Optional model-facing guidance.
	LLMHint string

	// RetentionHours: see Directory.RetentionHours. Zero = no sweep.
	RetentionHours int

	// Scope: see Directory.Scope. Default ScopeNone (no scoping).
	Scope DirectoryScope
}

// DirectoryDef is the wire format sent in SyncRequest.
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
	Access      Access // who may subscribe via topic_{slug}.subscribe(); default AccessUser
	// PerUser forbids broadcast: Publish panics, only PublishToUser delivers
	// (to the named user's subscribed conversations). Use for personal feeds
	// (reminders, alerts) where a broadcast would leak across users.
	PerUser bool
}

// TopicDef is the wire format sent in SyncRequest.
type TopicDef struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	LLMHint     string `json:"llmHint,omitempty"`
	Access      Access `json:"access"`
	PerUser     bool   `json:"perUser,omitempty"`
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

// PrintRequest is the body for POST /api/agent/print.
type PrintRequest struct {
	Parts          []DisplayPart `json:"parts"`
	Topic          string        `json:"topic,omitempty"`          // empty = direct to conversation
	ConversationID string        `json:"conversationId,omitempty"` // set for direct prints
	RunID          string        `json:"runId,omitempty"`          // originating run, used to sort ephemerals after their run's assistant messages
	UserID         string        `json:"userId,omitempty"`         // topic publish scoped to this user's subscribed conversations (PublishToUser)
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
	Slug     string // unique per agent; binds as mcp_{slug} in run_js
	Name     string
	URL      string
	AuthMode MCPAuth
	AuthURL  string
	TokenURL string
	Scopes   []string
	// AuthInjection picks how the stored credential is added to each MCP
	// HTTP call: bearer header (default), custom header, query parameter,
	// or path prefix. Mirrors Connection.AuthInjection.
	AuthInjection AuthInjection
	Access        Access // who may invoke mcp_{slug}; default AccessUser
}

// MCPDef is the wire format used by PUT /api/agent/mcp-servers/{slug} and
// (with Slug populated) by SyncRequest.MCPServers. Slug is sent in the URL
// for the per-slug PUT and in the body for the bulk sync.
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

// MCPToolSchema is a discovered MCP tool schema returned in SyncResponse.
type MCPToolSchema struct {
	ServerSlug  string          `json:"serverSlug"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPAuthStatus reports auth state for an MCP server.
type MCPAuthStatus struct {
	Slug       string  `json:"slug"`
	AuthMode   MCPAuth `json:"authMode"`
	Authorized bool    `json:"authorized"`
	AuthURL    string  `json:"authUrl,omitempty"`
	// Instructions is the server-level description the remote MCP server
	// advertised in its initialize result (the spec's `instructions`
	// field). Empty when the server set none. Rendered next to
	// mcp_<slug> in the prompt so the model knows what the server is for.
	Instructions string `json:"instructions,omitempty"`
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

// --- Sync / wire types (shared between agentsdk client and airlock server) ---

// SyncRequest is the body for PUT /api/agent/sync.
type SyncRequest struct {
	Version          string               `json:"version"`
	Description      string               `json:"description,omitempty"`
	Emoji            string               `json:"emoji,omitempty"`
	Tools            []ToolDef            `json:"tools,omitempty"`
	Webhooks         []WebhookDef         `json:"webhooks"`
	ScheduleHandlers []ScheduleHandlerDef `json:"scheduleHandlers"`
	Routes           []RouteDef           `json:"routes,omitempty"`
	Topics           []TopicDef           `json:"topics,omitempty"`
	MCPServers       []MCPDef             `json:"mcpServers,omitempty"`
	Connections      []ConnectionDef      `json:"connections,omitempty"`
	ExecEndpoints    []ExecEndpointDef    `json:"execEndpoints,omitempty"`
	EnvVars          []EnvVarDef          `json:"envVars,omitempty"`
	Directories      []DirectoryDef       `json:"directories,omitempty"`
	Instructions     []InstructionDef     `json:"instructions,omitempty"`
	ModelSlots       []ModelSlotDef       `json:"modelSlots,omitempty"`
}

// EnvVarDef is the wire format used by PUT /api/agent/env-vars/{slug}
// and (with Slug populated) by SyncRequest.EnvVars. Mirrors the
// agentsdk.EnvVar struct one-to-one.
type EnvVarDef struct {
	Slug        string `json:"slug,omitempty"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
	Default     string `json:"default,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}

// EnvVarValueResponse is the wire body of GET /api/agent/env-vars/{slug}
// — the operator-supplied value for one declared env var (or 404 if no
// value is configured).
type EnvVarValueResponse struct {
	Value string `json:"value"`
}

// ExecRequest is the wire body of POST /api/agent/exec/{slug}. Stdin
// arrives base64-encoded because it can be raw bytes and JSON can't
// carry those directly. TimeoutMs of 0 means "use the server default".
type ExecRequest struct {
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	StdinB64  string   `json:"stdinB64,omitempty"`
	TimeoutMs int64    `json:"timeoutMs,omitempty"`
}

// SealRequest / SealResponse are the wire bodies of POST /api/agent/seal:
// the agent posts plaintext it generated at runtime, airlock returns
// an opaque sealed blob bound to this agent's ID.
type SealRequest struct {
	Plaintext string `json:"plaintext"`
}

type SealResponse struct {
	Sealed string `json:"sealed"`
}

// UnsealRequest / UnsealResponse are the wire bodies of POST /api/agent/unseal:
// the agent posts a previously-sealed blob, airlock returns the
// plaintext (only if the blob was sealed for this same agent).
type UnsealRequest struct {
	Sealed string `json:"sealed"`
}

type UnsealResponse struct {
	Plaintext string `json:"plaintext"`
}

// SessionCompactRequest is the wire body of
// POST /api/agent/session/{convID}/compact: the agent posts the
// summarized message tail it wants to keep, plus a count of tokens the
// summarization freed, and airlock writes a checkpoint marker row
// followed by the summary.
type SessionCompactRequest struct {
	Summary     []session.Message `json:"summary"`
	TokensFreed int               `json:"tokensFreed"`
}

// Instruction is the self-contained declaration passed to agent.AddInstruction.
// The Text fragment is appended to the system prompt for runs whose caller
// access matches one of the listed Access levels. Empty Access slice means
// "applies to every access level."
type Instruction struct {
	Text   string
	Access []Access
}

// InstructionDef is the wire format sent in SyncRequest.
type InstructionDef struct {
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
//
// The agent renders its own system prompt per run from PromptData
// (platform-side data) plus its in-memory registrations. Airlock no
// longer ships a pre-rendered SystemPrompt: per-run rendering is the
// only way to express per-user sibling visibility without exploding
// the wire payload into N variants.
type SyncResponse struct {
	// PromptData carries the slice of system-prompt input the agent
	// can't derive locally: dashboard / route URLs, the full sibling
	// address book with their published tool schemas. Required. An
	// older agentsdk that doesn't know about PromptData would have
	// produced an empty system prompt; the new agentsdk's
	// applySyncResponse panics on a zero-value PromptData with a
	// clear "your airlock is newer than your agentsdk" message.
	PromptData PromptData `json:"promptData"`

	MCPAuthStatus []MCPAuthStatus `json:"mcpAuthStatus,omitempty"`
	// MCPSchemas carries discovered tool schemas per MCP server slug.
	// Airlock populates these from its server-side discovery cache so the
	// agent's VM can install one typed JS method per tool on each
	// `mcp_{slug}` object — no per-run discovery round-trips.
	MCPSchemas map[string][]MCPToolSchema `json:"mcpSchemas,omitempty"`
	// PublicStorageBase is the URL prefix at which directories are reachable
	// on the agent's subdomain, ending without a trailing slash. Callers
	// join with '/' and the storage path (e.g. "reports/q1.csv") to
	// construct a URL: "https://{slug}.{domain}/__air/storage/reports/q1.csv".
	// The proxy enforces the directory's Read cap at fetch time — public
	// dirs serve unauthenticated, user/admin dirs require subdomain login
	// (redirect-on-missing-cookie).
	PublicStorageBase string `json:"publicStorageBase,omitempty"`
}

// PromptData is the platform-supplied slice of the prompt-render
// input — everything the agent can't compute locally from its own
// in-memory registrations.
type PromptData struct {
	// AgentDashboardURL points at the agent's settings page in the
	// Airlock UI; the prompt tells the LLM to direct users there when
	// a connection or MCP server needs OAuth.
	AgentDashboardURL string `json:"agentDashboardUrl"`

	// AgentRouteURL is the agent's public subdomain (scheme + host +
	// optional port). The prompt embeds it for "share file at this
	// URL" guidance. Derived server-side because the scheme/port
	// logic lives in airlock's PUBLIC_URL parsing.
	AgentRouteURL string `json:"agentRouteUrl"`

	// Siblings is the FULL configured sibling list with each one's
	// tool schemas. Static at sync time (changes when the operator
	// edits the address book). Per-user visibility is layered on at
	// dispatch via PromptInput.VisibleSiblings.
	Siblings []SiblingInfo `json:"siblings,omitempty"`

	// Capabilities are the model slots Airlock has bound for this
	// agent (agent override → system default). Each bool is true iff
	// some model is bound for that slot — the prompt branches on
	// these to avoid recommending builtins that would 4xx at
	// runtime (e.g. analyzeImage on an agent with no vision model).
	Capabilities Capabilities `json:"capabilities,omitempty"`

	// SupportedModalities is the chat model's declared input
	// modality list ("text", "image", "pdf", "audio", "video") at
	// sync time. PromptInput.SupportedModalities overrides per-run
	// when set (the run-time value reflects the actual model that
	// will serve THIS turn, which can differ from sync if the agent
	// uses run.LLM(slug=...) elsewhere). The prompt template uses
	// whichever the agent has on hand.
	SupportedModalities []string `json:"supportedModalities,omitempty"`
}

// Capabilities is a one-bool-per-slot capability matrix. Field names
// mirror ModelCapability constants (Vision/Transcription/Speech/
// Embedding/Image) with one extra for the web-search service slot,
// which is a non-LLM service but follows the same agent-override →
// system-default resolution pattern.
type Capabilities struct {
	Vision        bool `json:"vision,omitempty"`        // chat with images — analyzeImage / multimodal attachToContext
	Transcription bool `json:"transcription,omitempty"` // speech-to-text — voice-note auto-transcribe + transcribe()
	Speech        bool `json:"speech,omitempty"`        // text-to-speech — speech()
	Embedding     bool `json:"embedding,omitempty"`     // vector embeddings — embed()
	Image         bool `json:"image,omitempty"`         // image generation — generateImage()
	Search        bool `json:"search,omitempty"`        // web search — webSearch()
}

// SiblingInfo describes one sibling agent in the caller's address
// book. Travels in PromptData.Siblings.
type SiblingInfo struct {
	// ID is the canonical, rename-safe identifier. MCP outbound calls
	// use the UUID in the URL path so a sibling rename doesn't break
	// in-flight bindings.
	ID uuid.UUID `json:"id"`
	// Slug is the human-readable binding name — appears in the prompt
	// and as the `agent_<slug>` namespace on this agent's VM.
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Tools       []MCPToolSchema `json:"tools,omitempty"`
}

// ToolDef describes a registered tool sent during sync. Carries the JSON
// schemas for input and output so Airlock can render TypeScript signatures
// in the system prompt and surface them in the UI.
type ToolDef struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	LLMHint       string            `json:"llmHint,omitempty"`
	Access        Access            `json:"access"`
	InputSchema   json.RawMessage   `json:"inputSchema,omitempty"`
	OutputSchema  json.RawMessage   `json:"outputSchema,omitempty"`
	InputExamples []json.RawMessage `json:"inputExamples,omitempty"`
}

// RouteDef is a custom HTTP route definition sent during sync.
type RouteDef struct {
	Path        string `json:"path"`
	Method      string `json:"method"`
	Access      Access `json:"access"`
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

// ScheduleHandlerDef is one registered cron/schedule handler sent during sync.
// Kind is "cron" or "schedule"; Recurrence is the cron expression for crons,
// empty for schedules.
type ScheduleHandlerDef struct {
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`
	Recurrence  string `json:"recurrence,omitempty"`
	TimeoutMs   int64  `json:"timeoutMs"`
	Description string `json:"description,omitempty"`
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

// HTTPRequest is the body for POST /api/agent/http.
type HTTPRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"` // default: GET
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // seconds, default: 30, max: 120
	SaveAs  string            `json:"saveAs,omitempty"`  // save response body to S3 at this key (binary-safe)
	Raw     bool              `json:"raw,omitempty"`     // skip HTML→markdown conversion for HTML responses
	// AllHeaders returns every upstream response header. Default (false)
	// returns only the curated few an agent reasons about; the rest
	// (CSP, Via, Alt-Svc, telemetry) are noise that burns context.
	AllHeaders bool `json:"allHeaders,omitempty"`
}

// HTTPResponse is returned from POST /api/agent/http.
type HTTPResponse struct {
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"contentType"` // original upstream Content-Type
	// Size is the byte length of the content the agent can act on — the
	// inline body, the converted markdown, or the object written to
	// SavedTo. Always populated (never the upstream Content-Length,
	// which is 0 for chunked/unknown).
	Size int `json:"size"`
	// BodyPreview is the head (~1 KB) of a saved text/markdown body so
	// the result is legible without a second fileRead. Empty for binary
	// or inline (Body carries the whole thing) responses.
	BodyPreview string `json:"bodyPreview,omitempty"`
	SavedTo     string `json:"savedTo,omitempty"` // S3 key if body was auto-saved
	Note        string `json:"note,omitempty"`    // human-readable note about transformations applied (e.g. HTML→markdown conversion)
}

// ProxyRequest is the body for POST /api/agent/proxy/{slug}.
//
// Headers are per-call request headers, merged per-key on top of the
// connection's declared Headers (which themselves sit on top of the
// platform baseline). Set a value to the empty string to suppress a key
// set by a lower layer. omitempty: a call that doesn't need custom
// headers can simply omit the field.
type ProxyRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ShareFileRequest is the body for POST /api/agent/storage/share.
// Path is an S3-style storage path (no leading slash); ExpiresSeconds
// caps how long the returned URL is valid for. Server defaults to 1h if
// 0, caps at 24h.
type ShareFileRequest struct {
	Path           string `json:"path"`
	ExpiresSeconds int64  `json:"expiresSeconds,omitempty"`
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

// LogLevel categorizes a builder-emitted log line. UI can color/filter on it;
// the wire format stores it explicitly so the level isn't lost in a flat string.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// LogEntry is one builder-emitted line: a level and a message. The wire
// format used by /api/agent/run/complete; also the in-memory shape on the run.
type LogEntry struct {
	Level   LogLevel `json:"level"`
	Message string   `json:"message"`
}

// Error kinds passed in RunCompleteRequest.ErrorKind. The agentsdk side
// classifies structurally — by call-site, not by error string — so airlock
// can avoid pattern-matching at all.
const (
	// ErrorKindPlatform: failure upstream of the agent's own code. LLM
	// provider 4xx, sol/goai stream errors, request transport (body read).
	// The agent's code couldn't have prevented or fixed this — the "Fix
	// this error" workflow on the run page is hidden for these.
	ErrorKindPlatform = "platform"

	// ErrorKindAgent: failure from agent-defined code paths. Webhook/cron
	// handlers returning err, panics in user code recovered by the SDK,
	// post-LLM bookkeeping that hit something the agent owns. The Fix
	// workflow targets exactly these.
	ErrorKindAgent = "agent"
)

// RunCompleteRequest is the body for POST /api/agent/run/complete.
type RunCompleteRequest struct {
	RunID string `json:"runId"`
	// Status is "success" | "error" | "suspended" | "timeout" | "tool_errors".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// ErrorKind is set when Status == "error" and disambiguates platform
	// vs agent failure for the UI. Empty otherwise.
	ErrorKind  string          `json:"errorKind,omitempty"`
	PanicTrace string          `json:"panicTrace,omitempty"`
	Actions    json.RawMessage `json:"actions"`
	Logs       []LogEntry      `json:"logs,omitempty"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
}
