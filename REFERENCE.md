# agentsdk — API reference

This file documents the agentsdk SDK surface: every `Register*` API, the
LLM-calling helpers, storage, seal/unseal, built-in JS bindings, and the
runtime contracts an agent must satisfy. It is the answer to *"what does
the SDK give me?"*.

It is consumed two ways, and both should treat it as authoritative:

- **The Airlock agent-builder** reads it before generating or upgrading
  agent code.
- **You, by hand** — point your editor's AI at
  `.airlock/toolchain/skills/agentsdk/SKILL.md`; the same reference ships in
  the `agentsdk` module.

For the orthogonal half — *how* to wire the SDK together inside a real
agent (file layout, MVC, build chain, NOTES.md convention, UI design
rules) — read **`AGENTS.md` at the agent's repo root**. That file is
materialised by the Airlock scaffold once and stays with the agent; this
file is the canonical SDK reference.

## Mental model

An agent has definition and runtime phases. `agentsdk.New` creates declaration
state only: no runtime environment, database, network, or migrations. Factories
register capabilities, construct services, and inject returned handles.
`Agent.DB()` is late-bound: it can be wired now, but operations require startup.

`Agent.Manifest()` validates and freezes registrations and returns the complete
canonical declaration used for sync. `AIRLOCK_AGENT_MODE=manifest` makes
`Serve` emit it as one JSON line without starting runtime dependencies.

Normally, `Serve` freezes declarations, starts runtime dependencies and
migrations, syncs with Airlock, runs named process-local `OnStart` hooks in
registration order, then serves. Later registration panics. Hooks are for
disposable local initialization; durable work belongs in a registered job.

At runtime the LLM does **not** see your Go functions directly. It sees one
tool, `run_js`, a JavaScript VM. Everything you register with `RegisterTool`
becomes a typed JS global inside that VM; the LLM writes JS that calls your
tools. Airlock renders your `In`/`Out` Go structs as TypeScript signatures in
the system prompt and validates arguments before your `Execute` runs. You never
touch the JS engine — you write plain typed Go.

Airlock is the runtime around the agent: auth, storage (S3-like), the LLM
proxy, credential injection for outbound HTTP/MCP, conversation history,
triggers (webhooks/crons/bridges), and the per-agent Postgres schema.

## Deep-dive references

Read the relevant companion at its build-container path:

- **[Object storage](reference/files.md)** (`/libs/agentsdk/reference/files.md`) — `RegisterDirectory`, the
  trusted Go file API, gating untrusted (LLM-supplied) paths with
  `ResolveFilePath`, shelling out to CLIs over storage, presigned URLs.
- **[Live integrations](reference/integrations.md)** (`/libs/agentsdk/reference/integrations.md`) — validate configured
  connections and MCP servers without retrieving credentials.
- **[Interactive authentication](reference/auth-web.md)** (`/libs/agentsdk/reference/auth-web.md`) — login flows (one-time
  code / password / click) driven from an admin web page, ending in `Seal`.
- **[Database](reference/database.md)** (`/libs/agentsdk/reference/database.md`) — Postgres: goose migrations, sqlc
  queries, database-only Go migrations, build-time validation, and when to use jobs instead.
- **[Connectors](reference/connectors.md)** (`/libs/agentsdk/reference/connectors.md`) — typed remote-machine
  contracts, `RegisterConnector`, command and directory clients, connector runtime, settings, activation, and services.

## Verifying a build

After writing code, run the SDK-owned build chain:

```bash
go tool air build
```

This reconciles module sums, conditionally generates sqlc output when
`db/queries/*.sql` exists, generates templ output, compiles Tailwind and
DaisyUI, runs `go test -p=1 -count=1 ./...`, and verifies the Go binary without
leaving it in the source tree.

Generated `*_templ.go`, `views/static/app.css`, `internal/db/*` except
`internal/db/doc.go`, and root binaries are gitignored. Commit source inputs;
builds regenerate outputs.

`agenttest.New(t, factory)` invokes the factory first with runtime environment
cleared, then provisions its mock and database, starts, migrates, syncs, and
runs `OnStart` hooks. It returns an agent ready for `DB` and `Handler`;
`Env.Airlock` records platform calls. Tests needing only the HTTP mock can use
`agenttest.NewMockAirlock`; wire payloads remain an SDK runtime detail.

Authenticated in-process handler tests attach caller state without private
transport headers:

```go
user := agentsdk.User{ID: "00000000-0000-0000-0000-000000000001"}
req := httptest.NewRequest(http.MethodGet, "/", nil)
req = req.WithContext(agenttest.WithUser(req.Context(), user)) // AccessUser
env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
```

`agenttest.WithCaller(ctx, user, access)` selects explicit access. Identity and
access are independent; plain contexts are public. Context values do not cross
`httptest.NewServer`.

## Design principle: register granular tools

Give the LLM the same useful operations as every route or integration. Prefer
`importPlaylist`, `listSongs`, `getSong`, and `voteSong` over one bulk-only tool;
the agent must be able to inspect and query the data it changes.

## Worked example

A connection, direct dependency injection, and a granular tool. Other
`Register*` methods use the same constructed-receiver pattern.

```go
// main.go
package main

import (
    "agent/spotify"
    "github.com/airlockrun/agentsdk"
    "github.com/airlockrun/goai/tool"
)

func main() {
    newAgent().Serve()
}

func newAgent() *agentsdk.Agent {
    agent := agentsdk.New(agentsdk.Config{
        Description: "Spotify agent — playback control and search",
    })

    spotifyConn := agent.RegisterConnection(&agentsdk.Connection{
        Slug:          "spotify",
        Name:          "Spotify",
        Description:   "Spotify Web API",
        BaseURL:       "https://api.spotify.com",
        AuthMode:      agentsdk.ConnectionAuthOAuth,
        AuthURL:       "https://accounts.spotify.com/authorize",
        TokenURL:      "https://accounts.spotify.com/api/token",
        Scopes:        []string{"user-read-playback-state", "user-modify-playback-state"},
        AuthInjection: agentsdk.AuthInjection{Type: agentsdk.AuthInjectBearer},
        LLMHint:       "All paths start with /v1/.",
        Access:        agentsdk.AccessUser,
    })

    spotifyService := spotify.NewService(spotifyConn)
    tools := spotify.NewTools(spotifyService)

    agent.RegisterTool(tool.Typed[spotify.SearchIn, spotify.SearchOut]("search_tracks").
        Description("Search Spotify tracks.").
        Execute(tools.SearchTracks).
        Build(), agentsdk.AccessUser)

    return agent
}
```

```go
// spotify/service.go
package spotify

import (
    "context"
    "net/url"

    "github.com/airlockrun/agentsdk"
)

type Service struct {
    connection *agentsdk.ConnectionHandle
}

func NewService(connection *agentsdk.ConnectionHandle) *Service {
    if connection == nil {
        panic("spotify: connection is required")
    }
    return &Service{connection: connection}
}

func (s *Service) SearchTracks(ctx context.Context, in SearchIn) (SearchOut, error) {
    body, err := s.connection.Request(ctx, agentsdk.RequestOpts{
        Path: "/v1/search?type=track&q=" + url.QueryEscape(in.Query),
    })
    if err != nil {
        return SearchOut{}, err
    }
    return SearchOut{Response: body}, nil
}
```

```go
// spotify/tools.go
package spotify

import (
    "context"
    "encoding/json"
)

type SearchIn struct {
    Query string `json:"query" jsonschema:"description=Search query"`
}
type SearchOut struct {
    Response json.RawMessage `json:"response"`
}

type Tools struct {
    service *Service
}

func NewTools(service *Service) *Tools {
    if service == nil {
        panic("spotify: service is required")
    }
    return &Tools{service: service}
}

func (t *Tools) SearchTracks(ctx context.Context, in SearchIn) (SearchOut, error) {
    return t.service.SearchTracks(ctx, in)
}
```

**Key patterns:**
- `RegisterConnection` returns `*ConnectionHandle`; use it for all API calls.
- `newAgent` injects handles into services and services into consumers, then
  registers bound receiver methods.
- `handle.Request(ctx, agentsdk.RequestOpts{Path: ...})` returns raw bytes.
  `RequestOpts.Method` defaults to `"GET"`; `Body` auto-encodes (struct → JSON,
  `[]byte`/`string` as-is, `nil` → no body); `Headers` is an optional
  `map[string]string`. Airlock injects credentials.
- `LLMHint` is appended to the connection block in the runtime system prompt.

---

# API reference

## Agent

```go
agent := agentsdk.New(agentsdk.Config{
    Description: "What this agent does — shown to users in the UI", // required, panics if empty
    Emoji:       "🎧",                                              // optional decorative glyph next to the agent in the UI; "" = none
})
agent.Serve() // starts HTTP server, blocks until shutdown
```

`agentsdk.New` is pure definition and wiring. `agent.DB()` returns a late-bound
`*AgentDB` for sqlc constructors; operations fail before startup.
`agent.OnStart(name, hook)` registers process-local initialization that runs in
order after sync and before readiness; use jobs for durable work.
`agent.Manifest()` freezes and returns the complete canonical declaration.
`AIRLOCK_AGENT_MODE=manifest` emits it offline; normally `Serve` freezes,
starts, migrates, syncs, runs hooks, and serves.

**Choosing `Emoji`:** every product on this platform is an agent, so the
emoji must distinguish *this* agent from all the others — pick one that
evokes its specific domain, never the generic "agent/AI" concept. Do
**not** use 🤖 ⚙️ 🧠 🦾 💬 or similar "it's a bot" glyphs; they're
noise when every entry in the list is a bot. A Spotify agent is 🎧, a
weather agent 🌦️, an invoicing agent 🧾, a calendar agent 📅. Think
"what is this agent *about*?", not "what is this agent?".

## Typed dependency composition

`newAgent` registers handles, constructs services, and injects them directly
into consumers. Use direct parameters for one or two dependencies and a
package-local constructor `Deps` struct for larger sets. There is no application-
wide dependency package or shared container. Reject nil requirements.

A domain package must never import a package that imports that domain.
`handlers` may import `spotify`, but `spotify` must not import `handlers`;
`newAgent` injects the same `*spotify.Service` into both consumers.

## RegisterTool

The LLM has one tool, `run_js`. `RegisterTool` exposes typed Go functions as JS
globals inside that VM. A tool is a `goai/tool.Tool`; build it from Go types with
`tool.Typed[In, Out]`. Each tool declares **input and output struct types**;
Airlock renders them as TypeScript signatures in the system prompt and
validates arguments before `Execute` runs.

`RegisterTool(t tool.Tool, access agentsdk.Access, opts ...agentsdk.RegisterOption)`
takes the access tier as a positional argument (empty defaults to `AccessUser`).
The same `tool.Tool` value also works as a sub-call tool in
`agent.GenerateText`/`agent.StreamText` — define a tool once, use it everywhere
(but prefer computing in Go and feeding the result into the prompt over giving a
sub-call model a tool — see "Calling LLMs from agent code").

```go
import "github.com/airlockrun/goai/tool"

type DoThingIn struct {
    Query string `json:"query" jsonschema:"description=Search text"`
    Limit int    `json:"limit,omitempty" jsonschema:"minimum=1,maximum=50"`
}
type DoThingOut struct {
    Hits []string `json:"hits"`
}

agent.RegisterTool(tool.Typed[DoThingIn, DoThingOut]("do_thing").
    Description("Short, action-oriented summary.").
    Execute(func(ctx context.Context, in DoThingIn) (DoThingOut, error) {
        return DoThingOut{Hits: []string{"one", "two"}}, nil
    }).
    Build(), agentsdk.AccessUser)
```

Model-only guidance that shouldn't appear in member-facing UIs goes through an
option: `agent.RegisterTool(t, agentsdk.AccessUser, agentsdk.WithLLMHint("expensive; cache results"))`.

The LLM sees:

```typescript
/** Short, action-oriented summary. */
declare function do_thing(args: { query: string; limit?: number }): { hits: string[] };
```

and calls it as `do_thing({query: "foo", limit: 5})`.

**Naming:** `snake_case` — matches LLM tool conventions and MCP. Built-in VM
bindings are `camelCase` (or `snake_prefix.camelMethod`) by design — that's how
the LLM tells platform primitives from agent-declared tools.

**`In` / `Out` struct rules:**
- `json` tags required. Add `jsonschema:"description=..."` for per-field docs.
- `omitempty` → `?` in TypeScript signature.
- Prefer `string` (RFC3339) over `time.Time` for dates — `json.Unmarshal` only
  accepts RFC3339 for `time.Time` and that surprises the LLM.
- No recursive types (the schema generator can't detect cycles); use
  `json:"-"` on cycle-closing fields.
- **Path fields use `agentsdk.FilePath` (or `[]agentsdk.FilePath`), not
  plain `string`.** FilePath carries a schema marker airlock uses to
  auto-copy files across A2A and external MCP boundaries — a sibling
  that calls your tool with `In.File: FilePath` gets your file copied
  into its own bucket, and `Out.Result: FilePath` lands in the caller's
  `siblings/{your-slug}/...`. Plain `string` paths are forwarded verbatim
  and resolve in the callee's own namespace — almost always a 404.
- **Directory fields use `agentsdk.DirPath`.** Auto-copy is intentionally
  unimplemented for directories (unbounded); for cross-agent directory
  semantics return `[]FilePath` so the caller picks exact files. Still
  preferred over `string` for the schema marker.
- Inside the tool body, convert when calling the trusted file API —
  `agent.OpenFile(ctx, string(in.Image))`. `FilePath`/`DirPath` are
  defined string types, not aliases, so the conversion is explicit.
- Binary data: write to storage with `agent.WriteFile`, return the path
  as `FilePath` (auto-copies). `FileInfo` is also fine when the LLM needs
  filename/size/contentType metadata — its `Path` field is already
  `FilePath`, so returning it (or embedding it in an output struct)
  triggers the same A2A auto-copy. Never base64 strings.

**Error handling:** return `error` from `Execute` — converted to a JS `throw`
inside `run_js`. Don't panic.

**Access:** required and explicit: `AccessUser`, `AccessAdmin`, or
`AccessPublic`.

**Optional:** `InputExamples: []In{...}` renders `@example` JSDoc lines
alongside the signature.

**No goja.** You never touch `*goja.Runtime`, `goja.FunctionCall`,
`vm.ToValue`, or `vm.NewGoError` — the SDK handles the VM boundary. You write
plain typed Go.

## AddInstruction — access-scoped system prompt fragments

Airlock already renders a base system prompt covering `run_js`, registered
tools (with TS signatures), MCP tools, connection helpers, public-caller safety
guards, and environment context (date, platform, conversation id). **Do not
re-declare any of that.** `AddInstruction` is only for content the baseline
can't infer:

- The agent's persona, tone, voice
- Domain rules the LLM can't deduce from the tool signatures
- Per-access behavior differences the user explicitly asked for

```go
agent.AddInstruction(&agentsdk.Instruction{
    Text: "You are a concise events assistant for the Berlin meetup. Answer in English.",
})
agent.AddInstruction(&agentsdk.Instruction{
    Text:   "Public callers: only answer questions about event times and location.",
    Access: []agentsdk.Access{agentsdk.AccessPublic},
})
```

Multiple calls accumulate in registration order. Empty `Access` slice = visible
to every caller. **Scope:** chat-style runs only (web UI, bridges). Webhooks and
jobs invoke your Go handler directly and never build a system prompt.

## RegisterWebhook

```go
agent.RegisterWebhook(&agentsdk.Webhook{
    Path: "github",
    Handler: func(ctx context.Context, data []byte, ew *agentsdk.EventWriter) error {
        return nil
    },
    Verify:      "hmac",                // required: "none" | "hmac" | "token" | "bearer" | "ed25519"
    Header:      "X-Hub-Signature-256", // signature header (required for hmac, optional for ed25519)
    Description: "GitHub push events",
})
```

Airlock verifies the request before the handler runs (the per-webhook secret it
manages): `hmac` (HMAC-SHA256 of the body, GitHub `sha256=` prefix tolerated),
`token` (`?token=`), `bearer` (`Authorization: Bearer`), `ed25519` (Discord-style
asymmetric over `timestamp‖body`, ±5-min skew). So the handler is trusted.
Use `Verify: "none"` explicitly for an unverified webhook. `Description` is
required; zero `Timeout` means two minutes and negative values are rejected.

## RegisterJob

Typed, versioned jobs provide durable background work, recurring cron enqueue,
and delayed enqueue. Delivery is at least once. Read
`/libs/agentsdk/reference/jobs.md` for registration, lifecycle operations,
idempotency, synchronous durable `JobContext.ReportProgress`, progress snapshots,
`JobHandle.Cron`, `JobHandle.EnqueueAt`, and operational data migrations.

## RegisterRoute — custom HTTP routes

Routes serve API endpoints or HTML pages directly from the agent, proxied via
the agent's subdomain.

```go
agent.RegisterRoute(&agentsdk.Route{
    Method: "GET",
    Path:   "/api/data",
    Handler: func(w http.ResponseWriter, r *http.Request) error {
        return json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
    },
    Access:      agentsdk.AccessUser,
    Description: "Get data",
})
```

**Always provide a Description.**

Return `agentsdk.NewHTTPError(status, publicMessage, cause)` for an intentional
4xx/5xx. The SDK sends only the public message and records the internal cause.
Other returned errors become a generic 500.

**Access:**
- `AccessUser` — **default choice.** Any agent member.
- `AccessAdmin` — for destructive/sensitive ops (config, delete, reset).
- `AccessPublic` — anyone, no auth. **Only when the user explicitly asks** for
  a public-facing page. Never default to public.

### HTML UI

agentsdk bundles htmx, the complete Lucide icon catalog, and immutable serving
for agent-owned static files. Read
`/libs/agentsdk/reference/html-ui.md` for the asset APIs, inline icon renderer,
and templ route example. UI structure and design rules live in the agent's
managed `AGENTS.md`; version-matched templ, htmx, DaisyUI, and Lucide references
live under `.airlock/toolchain/skills/`.

## RegisterConnection

```go
spotify := agent.RegisterConnection(&agentsdk.Connection{
    Slug:          "spotify",
    Name:          "Spotify",
    Description:   "Spotify Web API",
    BaseURL:       "https://api.spotify.com",
    AuthMode:      agentsdk.ConnectionAuthOAuth,
    AuthURL:       "https://accounts.spotify.com/authorize",
    TokenURL:      "https://accounts.spotify.com/api/token",
    Scopes:        []string{"user-read-playback-state"},
    AuthInjection: agentsdk.AuthInjection{Type: agentsdk.AuthInjectBearer},
    LLMHint:       "All paths start with /v1/.",
    Access:        agentsdk.AccessUser,
})

// Simple GET — Method defaults to "GET"
body, err := spotify.Request(ctx, agentsdk.RequestOpts{Path: "/v1/me/player"})

// POST/PUT with a body
spotify.Request(ctx, agentsdk.RequestOpts{
    Method: "PUT", Path: "/v1/me/player", Body: myStruct,
})

// Per-call headers (User-Agent override, conditional fetches, etc.)
spotify.Request(ctx, agentsdk.RequestOpts{
    Path:    "/v1/me/player",
    Headers: map[string]string{"If-None-Match": etag},
})
```

**`AuthMode`:** `ConnectionAuthOAuth`, `ConnectionAuthToken`,
`ConnectionAuthNone`.

**`AuthInjection.Type`** — how the proxy injects the credential into each
request:
- `agentsdk.AuthInjectBearer` — sets header `Authorization: Bearer {token}`
- `agentsdk.AuthInjectAPIKey` — sets header `{Name}: {token}` (`Name` defaults
  to `X-API-Key`)
- `agentsdk.AuthInjectPathPrefix` — prepends `/{token}` to the URL path. For
  example, with `BaseURL: "https://api.telegram.org"` and a stored token
  `bot123:abc`, a request to `/sendMessage` becomes
  `https://api.telegram.org/bot123:abc/sendMessage`. Used by APIs that carry
  credentials in the path (Telegram bot API: store the token as `bot{token}`
  so the prepended segment is `/bot{token}`).
- `agentsdk.AuthInjectQueryParam` — appends `?{Name}={token}` (or merges into
  the existing query string). `Name` is required. Used by APIs that
  authenticate via a query parameter (e.g. Google APIs with `?key={token}`).

**`LLMHint`** is appended to the runtime system prompt. Set when the API has
non-obvious conventions (path prefixes, special headers).

**JS bindings.** Each registered connection appears in the JS VM as
`conn.<slug>.request(method, path, body?, headers?)` and
`conn.<slug>.requestJSON(...)`. Both return an envelope: small responses
(≤ 8 KiB) come back inline (`body` / `data`); larger ones auto-spill to
`tmp/conn-{slug}-{callID}.bin` with `bodyPreview` + `bodySavedTo` set
(no `body`/`data`). Read the spilled payload with `air.fileRead(bodySavedTo)`.

## RegisterMCP

```go
github := agent.RegisterMCP(&agentsdk.MCP{
    Slug:     "github",
    Name:     "GitHub",
    URL:      "https://api.githubcopilot.com/mcp",
    AuthMode: agentsdk.MCPAuthOAuthDiscovery, // RFC 9728/8414 discovery + RFC 7591 DCR
    Access:   agentsdk.AccessUser,
})

resp, err := github.CallTool(ctx, "search_repos", map[string]any{"query": "test"})
if err != nil {
    if ae, ok := agentsdk.IsAuthRequired(err); ok {
        return fmt.Errorf("authorize at %s", ae.AuthURL)
    }
    return err
}
```

`MCPHandle.CallTool` returns `*AuthRequiredError` for unauthorized servers,
same as `ConnectionHandle.Request` — detect it with the same two-value
`agentsdk.IsAuthRequired(err)` pattern.

**`AuthMode`:** `MCPAuthOAuthDiscovery` (RFC 9728/8414 plus advertised RFC 7591
DCR), `MCPAuthOAuth` (manual URLs/client), `MCPAuthToken`, `MCPAuthNone`. Run
`go tool air mcp probe <url>` first. Use discovery only when DCR is `advertised`;
without DCR, manual OAuth with the reported endpoints is likely. `unknown` does
not mean no auth. See **`/libs/agentsdk/reference/integrations.md`**.

**`AuthInjection`** — same shape and options as
`RegisterConnection.AuthInjection` (bearer / api_key_header / path_prefix /
query_param). Set this when the MCP server expects the stored credential in a
non-Bearer position. Defaults to `Authorization: Bearer {token}` when unset, so
the standard MCP-over-OAuth case needs no extra config.

```go
agent.RegisterMCP(&agentsdk.MCP{
    Slug:          "exa",
    URL:           "https://mcp.exa.ai",
    AuthMode:      agentsdk.MCPAuthToken,
    AuthInjection: agentsdk.AuthInjection{Type: agentsdk.AuthInjectQueryParam, Name: "apiKey"},
})
```

## RegisterEnvVar — operator-configured environment variables

**Use sparingly.** For ordinary configuration, just define values in code —
env vars add operator burden for no benefit. For credentials that authenticate
proxied HTTP/MCP calls, prefer `RegisterConnection` / `RegisterMCP` with
`AuthInjection` — Airlock injects the credential at proxy time and the agent
code never touches it.

`RegisterEnvVar` is for the cases those don't cover: the user explicitly asked
for a configurable env var, or you're shelling out to a CLI that reads its
credentials from environment variables. Two flavours, controlled by `Secret`:

```go
// Plain config — operator sees/edits the value in the UI; Default ships a
// working setting.
region := agent.RegisterEnvVar(&agentsdk.EnvVar{
    Slug:        "aws_region",
    Description: "Default AWS region",
    Default:     "us-east-1",
    Pattern:     `^[a-z]{2}-[a-z]+-\d$`, // optional regex; rejected on save if no match
})

// Secret — write-only in the UI (rotate, no read-back). Auto-redacted on first
// Get(). Default is forbidden for secrets (the SDK panics).
accessKey := agent.RegisterEnvVar(&agentsdk.EnvVar{
    Slug: "aws_access_key_id", Description: "AWS IAM access key id",
    Secret: true, Pattern: `^AKIA[0-9A-Z]{16}$`,
})
```

Use the credential by reading it inside a tool and passing it to the subprocess
environment (`cmd.Env = append(os.Environ(), "AWS_ACCESS_KEY_ID="+ak, ...)`).

**`Get`'s contract**: returns the stored value (or `Default`, or `""` if
neither). The error is non-nil on transport/decrypt failure **and** when the
fetched value doesn't match the declared `Pattern` — including the empty
string. So if you declare `Pattern: "^.+$"` (or any tighter regex), `err != nil`
is exactly your "operator hasn't configured this yet" signal; no separate
`if v == ""` guard is needed. With no `Pattern`, `("", nil)` is a valid return
and your code decides what to do with it. Surface the slug in the error so the
operator knows what to set in the Environment tab.

## Seal / Unseal — persist secrets the agent generates at runtime

`RegisterEnvVar` is for secrets the **operator** supplies. `agent.Seal` /
`agent.Unseal` are the opposite: a secret the **agent itself produces** at
runtime and must reuse on later runs — a session token from an interactive
login, an OAuth refresh token, an API key the agent provisions. The agent never
holds the encryption key; Airlock encrypts/decrypts on its behalf and binds the
ciphertext to this agent, so no other agent can unseal it (and a leaked sealed
value is useless elsewhere).

```go
sealed, err := agent.Seal(ctx, sessionToken)  // plaintext -> opaque ciphertext
// ... persist `sealed` yourself ...
token,  err := agent.Unseal(ctx, sealed)       // ciphertext -> plaintext
```

**You own storage and cardinality** — Airlock only holds the key:
- **Agent-wide** single credential → store the sealed string as one blob via
  `agent.WriteFile` (a session string is opaque), or one row.
- **Per-user** — agents that let their own end users each link an account
  (SaaS-style, with public signup/login pages the agent serves) → a row in the
  agent's Postgres schema keyed by *the agent's own* `user_id`. Airlock knows
  nothing about those users; they're authenticated by the agent's own pages,
  so both the user identity and the per-user secret live in the agent's DB.

The plaintext is auto-registered for redaction (same heuristic as a Secret env
var), so it's stripped from LLM input. Never put a raw secret in `WriteFile`
without sealing first — storage is not encrypted at rest.

> Many credentials can't be minted headlessly — the login emits a one-time
> code, asks for a password, or needs a click. Drive that interactive step from
> an `AccessAdmin` `RegisterRoute` page, finish the login, and `Seal` the
> resulting long-lived credential. Full worked example (admin login page,
> sealing the session, per-user variant): **`/libs/agentsdk/reference/auth-web.md`**.

## RegisterModel — named model slots

Declare a named slot for every distinct LLM call **your own agent code makes** —
then invoke it through a getter (below). The admin picks a specific model per slot
in the Airlock UI. At runtime the slug resolves: slot binding → per-agent default
for the slot's capability → system default for that capability. The slot's
`Capability` is the single source of truth for the model type — the getters take
only a slug and read the capability from the slot.

Only register a slot you actually call. **Don't add a speculative slot "for future
use"**, and **don't register one for the agent's chat loop** — the conversation
that drives your `RegisterTool` tools runs on the agent's runtime model, which the
admin sets in the Models tab, not a `RegisterModel` slot. An agent whose own code
makes no LLM calls registers no slots at all (a slot with no matching getter call
is dead config and a misleading rebind knob in the UI).

Registration is required: every slug you pass to a model getter must be declared
with `RegisterModel` first. Calling a getter with an unregistered (or empty)
slug panics — a missing declaration is a programmer error, not a silent
fall-through to some default model.
Slugs must be valid and unique; capability constants and `Description` are required.

```go
agent.RegisterModel(&agentsdk.ModelSlot{
    Slug:        "summarize",
    Capability:  agentsdk.CapText,
    Description: "Short summaries for weekly reports",
})

model := agent.LLM(ctx, "summarize")
```

**Capabilities** (declared once on the slot; the getter just names the slug):
- `CapText` / `CapVision` → `agent.LLM(ctx, slug)`
- `CapImage` → `agent.ImageModel(ctx, slug)`
- `CapSpeech` → `agent.SpeechModel(ctx, slug)` (TTS)
- `CapTranscription` → `agent.TranscriptionModel(ctx, slug)` (STT)
- `CapEmbedding` → `agent.EmbeddingModel(ctx, slug)`
- `CapSearch` → `agent.WebSearch(ctx, slug, req)` (web search). The slot binds a *search
  provider* (+ optional model), not a chat model; the admin picks it in the Models tab. An
  unbound slot falls back to the agent's configured search provider, then the system
  default — same cascade as a model slot.

The built-in VM media helpers (analyze_image, generate_image, etc.) resolve the
system-default model by capability internally — that capability-routed path is
not exposed to agent code, which always goes through a registered slug.

## RegisterTopic

```go
alerts := agent.RegisterTopic(&agentsdk.Topic{
    Slug:        "alerts",
    Description: "System alerts",
    Access:      agentsdk.AccessUser,
})

alerts.Publish(ctx, []agentsdk.DisplayPart{
    {Type: "text", Text: "Daily report is ready"},
    {Type: "file", Source: "reports/daily.pdf", Filename: "report.pdf"},
})
```

The runtime LLM subscribes the current conversation via
`topic.<slug>.subscribe()`.
`Description` and `Access` are required.

## RegisterDirectory — file storage

The agent has its own **S3-like object storage** — there is no container
filesystem you expose to tools or the LLM. Every path is a slashless S3 key
(`uploads/x.csv`, `reports/q1.pdf`, `tmp/foo.png`); leading slashes are
rejected. Register a directory to declare per-capability access (`Read` /
`Write` / `List`) and an optional `LLMHint`. All three access values and the
model-facing `Description` are required:

```go
agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessUser, List: agentsdk.AccessUser,
    Description: "User-uploaded source files",
})
```

The **trusted Go file API** (`agent.ReadFile` / `WriteFile` / `OpenFile` /
`StatFile` / `ListDir` / `DeleteFile` / `CopyFile`) bypasses access checks — it's
your code. A path that arrives **from the LLM or any untrusted source** must be
resolved first with `agent.ResolveFilePath(ctx, llmPath, agentsdk.FileOperationRead)`.

→ Full directory ACL model, the complete file API, untrusted-path gating,
shelling out to a CLI over storage, and presigned URLs:
**`/libs/agentsdk/reference/files.md`**.

## Agent methods (ctx-first)

Tool, webhook, and timed handlers receive `context.Context` directly. Route
handlers use `r.Context()`. Pass that context through. Model calls and logging
are tracked in the Runs UI for the invoking handler; you never construct a Run
yourself.

```go
// Models — all ctx-first; slug must be declared with RegisterModel
agent.LLM(ctx, slug)                 // streaming chat model (CapText/CapVision)
agent.ImageModel(ctx, slug)
agent.SpeechModel(ctx, slug)         // TTS
agent.TranscriptionModel(ctx, slug)  // STT
agent.EmbeddingModel(ctx, slug)

// Logging — agent.Logger(ctx) returns a *zap.Logger. Bind it once at
// handler entry; the ctx is consumed there to resolve the run. Lines go
// to container stdout as structured JSON (run_id/agent_id tagged) and
// are kept by Airlock as the run's log record (a failed run's logs also
// feed the "Fix this error" builder). Use zap field constructors.
log := agent.Logger(ctx)
log.Info("imported rows", zap.Int("count", 42))
// Levels: Debug, Info, Warn, Error. import "go.uber.org/zap"

// Storage — trusted; no ResolveFilePath. See /libs/agentsdk/reference/files.md.
agent.OpenFile / ReadFile / WriteFile / StatFile / ListDir / DeleteFile / CopyFile
resolved, err := agent.ResolveFilePath(ctx, llmPath, agentsdk.FileOperationRead)
agent.DB() // late-bound *AgentDB handle; operations require a started runtime
```

`AuthRequiredError` from `ConnectionHandle.Request` means the user must
authorize. `agentsdk.IsAuthRequired(err)` returns `(*AuthRequiredError, bool)`
— call it with two-value assignment, never as a single boolean:

```go
resp, err := conn.Request(ctx, agentsdk.RequestOpts{Path: "/v1/me/playlists"})
if err != nil {
    if ae, ok := agentsdk.IsAuthRequired(err); ok {
        return fmt.Errorf("authorize at %s", ae.AuthURL)
    }
    return err
}
```

## Calling LLMs from agent code (agentsdk wrappers)

Crons, webhooks, and tool handlers make sub-model calls through the agent's own
generation methods, which mirror the `goai` functions but inject the run:

- `agent.GenerateText` / `agent.StreamText` (chat, with optional tools)
- `agent.GenerateImage` / `agent.GenerateSpeech` / `agent.Transcribe` / `agent.Embed`

Two things they handle for you:

1. **Model resolution.** Leave `Model` unset and the wrapper fills the agent's
   capability-routed proxy model (Airlock picks the per-agent default for that
   capability, then the system default). To pin a specific registered slot, set
   `Model: agent.LLM(ctx, "summarize")` (or `ImageModel`/`SpeechModel`/…) — each
   slug declared once with `RegisterModel`.
2. **Tool reach.** Tools you pass in `stream.Input.Tools` — typically the same
   `tool.Typed[...]` values you hand to `RegisterTool` — run under the call's
   run, so they can touch agent facilities (storage, events, sub-prompts). Set
   `MaxSteps > 1` to let the model call them in a loop.

Calls are proxied through Airlock so token usage is tracked. The wrappers are
always callable — from a cron, a webhook, or a detached goroutine with no run in
ctx — because they resolve a run (dispatcher → route-lazy → background) themselves.

**Plain text (default model):**

```go
import (
    "github.com/airlockrun/goai/message"
    "github.com/airlockrun/goai/stream"
)

func Summarize(ctx context.Context, in SummarizeIn) (SummarizeOut, error) {
    a := agentsdk.AgentFromContext(ctx)

    result, err := a.GenerateText(ctx, stream.Input{
        Messages: []message.Message{
            message.NewSystemMessage("You are a concise summarizer. 2-3 sentences."),
            message.NewUserMessage("Summarize: " + in.Text),
        },
    })
    if err != nil {
        return SummarizeOut{}, err
    }
    return SummarizeOut{Summary: result.Text}, nil
}
```

**With tools (the same tool.Tool you'd RegisterTool):**

```go
import "github.com/airlockrun/goai/tool"

lookup := tool.Typed[LookupIn, LookupOut]("lookup").
    Description("Look up a customer by id.").
    Execute(doLookup).
    Build()

result, err := a.GenerateText(ctx, stream.Input{
    MaxSteps: 4,
    Tools:    tool.Set{lookup.Name: lookup},
    Messages: []message.Message{message.NewUserMessage(question)},
})
```

> **Prefer computing in Go over handing the model a tool.** A tool earns its
> place only when the model must decide *whether* or *which* to call mid-
> conversation. Otherwise it's the slower, costlier path: a model→Airlock→model
> round-trip, it runs only if the model chooses to call it, and it spends tokens
> on the call/result envelope. When your handler already knows it needs the data
> — a lookup, a web search, a DB query — fetch it directly and put the result in
> the prompt (see **Web search** below for the pattern). Even *branching* rarely
> needs a tool — ask the model for a structured bool/enum decision and switch on
> it in Go (see **Branch without a tool** below). Reserve `Tools` for genuinely
> open-ended, multi-step loops where the model must pick and re-pick actions on
> its own.

**Structured output:**

```go
import (
    "github.com/airlockrun/goai/output"
    "github.com/airlockrun/goai/schema"
)

type SentimentResult struct {
    Sentiment  string  `json:"sentiment" description:"positive, negative, or neutral"`
    Confidence float64 `json:"confidence" description:"0.0 to 1.0"`
}

func AnalyzeSentiment(ctx context.Context, in SentimentIn) (SentimentResult, error) {
    a := agentsdk.AgentFromContext(ctx)

    result, err := a.GenerateText(ctx, stream.Input{
        Model: a.LLM(ctx, "sentiment"), // explicit slot
        Output: output.Object(output.ObjectOptions{
            Schema: schema.MustFromType(SentimentResult{}),
            Name:   "SentimentResult",
        }),
        Messages: []message.Message{
            message.NewUserMessage("Analyze sentiment: " + in.Text),
        },
    })
    if err != nil {
        return SentimentResult{}, err
    }
    raw, _ := json.Marshal(result.Output)
    var out SentimentResult
    _ = json.Unmarshal(raw, &out)
    return out, nil
}
```

`schema.MustFromType()` derives JSON schema from struct tags. Tools and
structured output can be combined: the model calls tools first, then produces
structured output on the final step. Other strategies: `output.Array`,
`output.Choice`, `output.JSON`, `output.Text` (default).

**Branch without a tool.** Instead of a tool for "let the model decide what
next", get a structured decision and branch in Go: control flow stays in your
code, it's one decision round-trip instead of a tool loop, and the decision
always happens.

```go
type Decision struct {
    GoodForJog bool `json:"good_for_jog" description:"true if the weather suits a jog"`
}

// 1. Structured yes/no, fed data you gathered in Go (parse res.Output as above).
res, _ := a.GenerateText(ctx, stream.Input{
    Output:   output.Object(output.ObjectOptions{Schema: schema.MustFromType(Decision{}), Name: "Decision"}),
    Messages: []message.Message{message.NewUserMessage("Good jogging weather?\n" + currentWeather())},
})
var d Decision
raw, _ := json.Marshal(res.Output); _ = json.Unmarshal(raw, &d)

// 2. Branch in Go; each arm is its own focused generation.
prompt := "Recommend a few mellow, melancholy songs."
if d.GoodForJog {
    prompt = "Write a short, upbeat nudge to go for a jog."
}
out, _ := a.GenerateText(ctx, stream.Input{Messages: []message.Message{message.NewUserMessage(prompt)}})
```

**Web search — search, then feed the results into the prompt.** Run the search
in Go with `agent.WebSearch(ctx, slug, req)` and put the results in the message.
Prefer this over handing the model a search *tool*: it's a single round-trip
(no model→Airlock→model detour), the search always runs (a tool the model may
decline to call doesn't), and you control exactly what enters the context. If
you really need the model to decide whether to search mid-conversation, wrap
`agent.WebSearch` in your own `RegisterTool`.

```go
import (
    "github.com/airlockrun/goai/message"
    "github.com/airlockrun/goai/stream"
    "github.com/airlockrun/sol/websearch"
)

// once, before Serve():
agent.RegisterModel(&agentsdk.ModelSlot{
    Slug: "research", Capability: agentsdk.CapSearch,
    Description: "Web search for the weekly digest",
})

func Digest(ctx context.Context, in DigestIn) (DigestOut, error) {
    a := agentsdk.AgentFromContext(ctx)

    found, err := a.WebSearch(ctx, "research", websearch.Request{Query: in.Topic, Count: 5})
    if err != nil {
        return DigestOut{}, err
    }

    result, err := a.GenerateText(ctx, stream.Input{
        Model: a.LLM(ctx, "assistant"),
        Messages: []message.Message{
            message.NewUserMessage("Using these search results:\n" + found.Text() +
                "\nWrite a short digest of: " + in.Topic),
        },
    })
    if err != nil {
        return DigestOut{}, err
    }
    return DigestOut{Summary: result.Text}, nil
}
```

`found.Text()` returns the provider's synthesized answer when available
(Grok/Gemini/Kimi) and otherwise a bulleted list of the results — so you don't
have to branch on `found.Synthesis` or format `found.Results` by hand.

## Capability namespaces

Every capability has a permanent namespace. Registered tools can use names such
as `output` without colliding with framework operations.

| Namespace | Source |
|---|---|
| `air.*` / `air__*` | framework operations |
| `tools.*` / `tool__*` | `RegisterTool` |
| `conn.*` / `conn__*` | `RegisterConnection` |
| `topic.*` / `topic__*` | `RegisterTopic` |
| `mcp.*` / `mcp__*` | `RegisterMCP` |
| `agent.*` / `agent__*` | A2A sibling capabilities |
| `run_js` (reserved) | the JS sandbox entry point |
| `agent__prompt` | open-ended A2A delegation |

Framework primitives (the runtime prompt describes each in detail).
**Availability**: *all* = every run; *authed* = non-public runs only; *admin* =
admin runs only; *per-dir* = bound only when a registered directory grants the
matching capability:

| Binding(s) | Purpose | Availability |
|---|---|---|
| `air.fileRead` `air.fileReadBytes` `air.fileWrite` `air.fileStat` `air.fileList` `air.fileDelete` `air.fileExists` `air.fileShareURL` | object storage; slashless paths; reads cap 16 MiB | per-dir |
| `air.fileReadRangeBytes` `air.fileGrep` `air.fileHead` `air.fileTail` `air.fileLines` | large-file reads | authed (read verbs also per-dir for public) |
| `air.fileEncode` `air.fileDecode` `air.fileDecodeText` | file transforms | authed |
| `air.fileEditLines` `air.fileSed` | streamed edits | authed |
| `air.analyzeImage` `air.transcribeAudio` `air.generateImage` `air.speak` `air.embed` | AI/media | authed |
| `air.output` | user-facing media output | all |
| `air.attachToContext` | attach a file to the conversation | authed |
| `air.httpRequest` `air.webSearch` | web | authed |
| `air.log` `console.log/warn/error` | logging | all |
| `air.queryDB` | read-only SQL | admin |
| `air.requestUpgrade` | self-rebuild | admin |

**Public-caller surface is much narrower.** A run triggered by an
`AccessPublic` caller only sees: `air.output`, `air.log`/`console`; the file API
verbs only where a registered directory grants `AccessPublic` for the matching
cap (no public dirs → no file API); and the `conn`/`mcp`/`topic`/registered
tools explicitly marked `Access: AccessPublic`. Everything in the *authed* /
*admin* rows above is bind-time-gated *out* — it doesn't exist in the JS runtime
for public runs, so prompt injection can't summon it.

So plan public flows around **single-purpose tools**, not a public directory the
LLM assembles primitives over. A `Tool` you control is one place to validate
input, sanitize the prompt, and decide what reaches the user; the LLM only sees
the verb you expose. E.g. for a public "AI image" feature, don't register a
public `generated/` dir and hope the LLM stitches `generateImage` + `fileWrite`
+ `fileShareURL` — register one `generate_public_image({prompt})` tool
(`Access: AccessPublic`) that generates internally, writes to an admin-only dir
(trusted Go bypasses `ResolveFilePath`), and returns only a presigned
`ShareFileURL`. Shrink the verbs, control the side effects, surface the URL.

---

# System dependencies — `setup.sh`

If the agent needs system packages or third-party binaries (`ffmpeg`,
`poppler-utils`, `bun`, `uv`, GitHub releases, pip packages — anything), create
`setup.sh` at the agent root. **Never create or modify a Dockerfile** — Airlock
generates it.

`setup.sh` runs as **root** at image-build time (and the same script bakes into
the runtime image, so what you install there is available to tools at runtime).
The base image is **Debian trixie**. Don't clean the apt cache; Airlock handles
that via BuildKit cache mounts.

```bash
# setup.sh — apt is the common case
apt-get update && apt-get install -y --no-install-recommends ffmpeg poppler-utils
```

`setup.sh` is not limited to apt. It can `curl`-bash an installer,
`pip install`, drop a release tarball under `/var/agent/bin/`, anything:

```bash
# setup.sh — non-apt example: bun (JS runtime, not in apt)
apt-get update && apt-get install -y --no-install-recommends curl unzip
mkdir -p /var/agent/bin
BUN_INSTALL=/var/agent/bin curl -fsSL https://bun.sh/install | bash
```

(Airlock builder: verify a binary works before relying on it inside a tool with
`sudo run-setup && /var/agent/bin/bun --version`. `sudo` is preconfigured for
`apt-get`, `apt-cache`, and `run-setup` only — no password. `run-setup` is a
fixed-path wrapper that execs `setup.sh` as root; you cannot `sudo
<other-command>` ad-hoc.)

## Persistent runtime state — `agent.SyncDown` / `agent.SyncUp`

`setup.sh` runs **once per image build** — anything it installs is frozen into
that image. For tools that *self-update* (`bun upgrade`, `uv self update`) or
download data that goes stale (GeoIP DBs, ClamAV signatures, cached ML weights),
the update happens at runtime in the container's writable layer and is lost when
the container gets reaped.

The fix is `agent.SyncDown` / `agent.SyncUp`: pair them to persist runtime
updates to the agent's S3-backed storage and pull them back at boot. The
container's local copy is the working copy; S3 is the durable record.

```go
// SyncDown(ctx, "state/bin/", "/var/agent/bin/")
//   for each remote file newer than local: download, atomic-rename,
//   chmod 0755, set local mtime to remote.
// SyncUp(ctx, "/var/agent/bin/", "state/bin/")
//   for each local file newer than remote: WriteFile, set local mtime
//   to the resulting S3 LastModified.

// Boot: restore process-local state after runtime sync and before readiness.
agent.OnStart("restore_bun", func(ctx context.Context) error {
    return agent.SyncDown(ctx, "state/bin/", "/var/agent/bin/")
})

// A recurring job self-updates, then pushes the new binary up.
refreshJob := agentsdk.RegisterJob(agent, &agentsdk.Job[struct{}, struct{}]{
    Name: "bun_refresh", Version: 1, Description: "Refresh the Bun binary.",
    Timeout: 10 * time.Minute, MaxAttempts: 3, MaxConcurrency: 1,
    Handler: func(ctx context.Context, _ agentsdk.JobContext, _ struct{}) (struct{}, error) {
        if err := exec.CommandContext(ctx, "/var/agent/bin/bun", "upgrade").Run(); err != nil {
            return struct{}{}, err
        }
        return struct{}{}, agent.SyncUp(ctx, "/var/agent/bin/", "state/bin/")
    },
})
refreshJob.Cron(&agentsdk.JobCron[struct{}]{
    Slug: "bun_refresh", Schedule: "0 3 * * 0", Description: "Refresh Bun weekly.",
})
```

Multi-replica is **last-writer-wins** (correct for self-updates — both replicas
converge on the same new binary). For shared mutable state with concurrent
writers, use the agent's Postgres schema instead — files are for blobs, rows are
for shared state.

Keep the persisted binaries in an **admin-only directory** with an `LLMHint`
that steers the model away (`"framework-managed binary cache; do not read,
write, or list"`); the trusted Go file API still reaches it freely. Use
`state/bin/` for executables, `state/data/` for everything else (GeoIP, ClamAV
sigs, ML weights, browser binaries) — it's just a convention so the LLM-facing
directory inventory reads cleanly.

**Startup budget:** the platform allows up to two minutes for a cold agent
runtime to become healthy. Named `OnStart` hooks block readiness and run in
registration order; return hydration errors so the process fails instead of
serving with incomplete local state. Keep hooks bounded by their context and
reserve durable business work for registered jobs. Never start runtime work in
the definition factory: manifest inspection invokes that factory offline.

---

# Database access

A full Postgres schema (usually with pgvector — pair vector columns with
`agent.EmbeddingModel(ctx, slug)`). Tables via goose migrations in
`db/migrations/`; queries via sqlc (`db/queries/` → generated `internal/db/`).
`AIRLOCK_DB_URL` is required at startup, not during definition. Pass the
late-bound `agent.DB()` to generated `New()` while wiring, but do not query from
the factory. `Start` opens and bounded-pings the pool and migrates before sync
and `OnStart`. **Always use sqlc** — never raw `db.QueryRow` / `db.Exec` strings
in Go.

```go
db := agent.DB()
queries := internaldb.New(db) // import "agent/internal/db" as internaldb
users, err := queries.ListActiveUsers(ctx)
```

→ Migration file format, numbering, database-only Go migrations (Tx vs NoTx),
operational jobs, and build-time validation:
**`/libs/agentsdk/reference/database.md`**.
