# agentsdk — API reference

This file documents the agentsdk SDK surface: every `Register*` API, the
LLM-calling helpers, storage, seal/unseal, built-in JS bindings, and the
runtime contracts an agent must satisfy. It is the answer to *"what does
the SDK give me?"*.

It is consumed two ways, and both should treat it as authoritative:

- **The Airlock agent-builder** reads it before generating or upgrading
  agent code.
- **You, by hand** — point your editor's AI at this file (it ships in
  the `agentsdk` module) or read it directly when writing or modifying
  an agent.

For the orthogonal half — *how* to wire the SDK together inside a real
agent (file layout, MVC, build chain, NOTES.md convention, UI design
rules) — read **`AGENTS.md` at the agent's repo root**. That file is
materialised by the Airlock scaffold once and stays with the agent; this
file is the canonical SDK reference.

## Mental model

An agent is a normal Go program. In `main()` you construct an agent, *register*
capabilities on it (tools, connections, MCP servers, webhooks, crons, routes,
topics, storage directories), then call `agent.Serve()`, which starts an HTTP
server and blocks.

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

Four subsystems live in their own companion files to keep this reference small
enough to read in one pass. This file covers everything else in full and gives
each of the four a short stub at its API slot. **Read the companion when your
task touches it** (paths are where they live in the build container):

- **`/libs/agentsdk/llms/files.md`** — object storage: `RegisterDirectory`, the
  trusted Go file API, gating untrusted (LLM-supplied) paths with
  `CheckFileAccess`, shelling out to CLIs over storage, presigned URLs.
- **`/libs/agentsdk/llms/exec.md`** — `RegisterExecEndpoint`: running commands
  on a remote machine over SSH, plus the shared overflow-response shape
  (`*SavedTo` + `fileRead`) used by connections, exec, and `httpRequest`.
- **`/libs/agentsdk/llms/auth-web.md`** — interactive login flows (one-time
  code / password / click) driven from an admin web page, ending in `Seal`.
- **`/libs/agentsdk/llms/database.md`** — Postgres: goose migrations, sqlc
  queries, Go migrations, build-time validation.

## Verifying a build

After writing code, generate templ output and compile:

```bash
go tool templ generate
go build ./...
```

Run `sqlc generate` only if you created or changed `.sql` files in
`db/queries/`. The Docker build re-runs `templ generate` and `tailwindcss`, so
`*_templ.go` and `views/static/app.css` are regenerated there and not
committed. It does **not** run sqlc, so the generated `internal/db/` **is**
committed (and updated whenever you change a query) — otherwise a fresh-clone
`docker build` would fail to find the package.

## Design principle: always register granular tools

Whenever you build a feature (route, cron, webhook, connection), also register
**granular tools** giving the LLM the same data and operations.

Bad: only `importPlaylist` (bulk insert). The LLM can import but can't inspect
or query.

Good: `importPlaylist`, `listSongs`, `getSong`, `voteSong`. Now the LLM can
answer "which song has the most votes?" through `run_js`.

Think: "what would the LLM need to call to be a helpful conversational
assistant in this domain?" and register those tools.

## Worked example

A connection + dependency injection + granular tools. Routes, crons, webhooks
and the rest follow the same `Register*` shape — register the capability in
`main()`, keep the logic in a domain package, retrieve handles via
`GetDeps`.

```go
// main.go
package main

import (
    "agent/deps"
    "agent/spotify"
    "github.com/airlockrun/agentsdk"
)

func main() {
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

    agent.Deps = &deps.Deps{Spotify: spotifyConn}

    agent.RegisterTool(&agentsdk.Tool[spotify.SearchIn, spotify.SearchOut]{
        Name: "search_tracks", Description: "Search Spotify tracks.",
        Execute: spotify.SearchTracks, Access: agentsdk.AccessUser,
    })
    agent.RegisterTool(&agentsdk.Tool[spotify.PlayIn, spotify.PlayOut]{
        Name: "play", Description: "Start or resume playback.",
        Execute: spotify.Play, Access: agentsdk.AccessUser,
    })

    agent.Serve()
}
```

```go
// deps/deps.go
package deps

import "github.com/airlockrun/agentsdk"

type Deps struct {
    Spotify *agentsdk.ConnectionHandle
}
```

```go
// spotify/tools.go
package spotify

import (
    "context"
    "encoding/json"
    "fmt"
    "net/url"

    "agent/deps"
    "github.com/airlockrun/agentsdk"
)

type Track struct{ Name, URI, Artist string }

type SearchIn struct {
    Query string `json:"query" jsonschema:"description=Search query"`
    Limit int    `json:"limit,omitempty" jsonschema:"minimum=1,maximum=50"`
}
type SearchOut struct {
    Tracks []Track `json:"tracks"`
}

func SearchTracks(ctx context.Context, in SearchIn) (SearchOut, error) {
    d := agentsdk.GetDeps[*deps.Deps](ctx)
    if in.Limit <= 0 {
        in.Limit = 10
    }
    body, err := d.Spotify.Request(ctx, agentsdk.RequestOpts{
        Path: fmt.Sprintf("/v1/search?type=track&limit=%d&q=%s", in.Limit, url.QueryEscape(in.Query)),
    })
    if err != nil {
        return SearchOut{}, err
    }
    var raw struct {
        Tracks struct {
            Items []Track `json:"items"`
        } `json:"tracks"`
    }
    if err := json.Unmarshal(body, &raw); err != nil {
        return SearchOut{}, err
    }
    return SearchOut{Tracks: raw.Tracks.Items}, nil
}

type PlayIn struct{ TrackURI string `json:"trackURI,omitempty"` }
type PlayOut struct{ OK bool `json:"ok"` }

func Play(ctx context.Context, in PlayIn) (PlayOut, error) {
    d := agentsdk.GetDeps[*deps.Deps](ctx)
    var body any
    if in.TrackURI != "" {
        body = map[string]any{"uris": []string{in.TrackURI}}
    }
    if _, err := d.Spotify.Request(ctx, agentsdk.RequestOpts{
        Method: "PUT", Path: "/v1/me/player/play", Body: body,
    }); err != nil {
        return PlayOut{}, err
    }
    return PlayOut{OK: true}, nil
}
```

**Key patterns:**
- `RegisterConnection` returns `*ConnectionHandle`; use it for all API calls.
- `agent.Deps` stores handles; tool funcs retrieve via `agentsdk.GetDeps[*deps.Deps](ctx)`.
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

**Choosing `Emoji`:** every product on this platform is an agent, so the
emoji must distinguish *this* agent from all the others — pick one that
evokes its specific domain, never the generic "agent/AI" concept. Do
**not** use 🤖 ⚙️ 🧠 🦾 💬 or similar "it's a bot" glyphs; they're
noise when every entry in the list is a bot. A Spotify agent is 🎧, a
weather agent 🌦️, an invoicing agent 🧾, a calendar agent 📅. Think
"what is this agent *about*?", not "what is this agent?".

## Agent.Deps — dependency injection

`Deps` lives in its own `deps` package so `main.go` and every domain
package can import the same type (see Project layout).

```go
// deps/deps.go
package deps

type Deps struct {
    Gmail   *agentsdk.ConnectionHandle
    GitHub  *agentsdk.MCPHandle
    Reports *agentsdk.TopicHandle
}
```

```go
// main.go
agent.Deps = &deps.Deps{
    Gmail:   agent.RegisterConnection(&agentsdk.Connection{Slug: "gmail", /* ... */}),
    GitHub:  agent.RegisterMCP(&agentsdk.MCP{Slug: "github", /* ... */}),
    Reports: agent.RegisterTopic(&agentsdk.Topic{Slug: "reports", Description: "Weekly reports"}),
}

// In any handler (any domain package):
func DoSomething(ctx context.Context, in SomeIn) (SomeOut, error) {
    d := agentsdk.GetDeps[*deps.Deps](ctx) // panics if type mismatch
    // use d.Gmail, d.GitHub, d.Reports
}
```

## RegisterTool

The LLM has one tool, `run_js`. `RegisterTool` exposes typed Go functions as JS
globals inside that VM. Each tool declares **input and output struct types**;
Airlock renders them as TypeScript signatures in the system prompt and
validates arguments before `Execute` runs.

```go
type DoThingIn struct {
    Query string `json:"query" jsonschema:"description=Search text"`
    Limit int    `json:"limit,omitempty" jsonschema:"minimum=1,maximum=50"`
}
type DoThingOut struct {
    Hits []string `json:"hits"`
}

agent.RegisterTool(&agentsdk.Tool[DoThingIn, DoThingOut]{
    Name:        "do_thing",
    Description: "Short, action-oriented summary.",
    Execute: func(ctx context.Context, in DoThingIn) (DoThingOut, error) {
        return DoThingOut{Hits: []string{"one", "two"}}, nil
    },
    Access: agentsdk.AccessUser,
})
```

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

**Access:** `AccessUser` (default), `AccessAdmin`, `AccessPublic`.

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
to every caller. **Scope:** chat-style runs only (web UI, bridges) — webhooks
and crons invoke your Go handler directly and never build a system prompt.

## RegisterWebhook

```go
agent.RegisterWebhook(&agentsdk.Webhook{
    Path: "github",
    Handler: func(ctx context.Context, data []byte, ew *agentsdk.EventWriter) error {
        return nil
    },
    Verify:      "hmac",                // "none" | "hmac" | "token" | "bearer" | "ed25519" (default "none")
    Header:      "X-Hub-Signature-256", // signature/token header (hmac/ed25519)
    Description: "GitHub push events",
    Access:      agentsdk.AccessUser,
})
```

Airlock verifies the request before the handler runs (the per-webhook secret it
manages): `hmac` (HMAC-SHA256 of the body, GitHub `sha256=` prefix tolerated),
`token` (`?token=`), `bearer` (`Authorization: Bearer`), `ed25519` (Discord-style
asymmetric over `timestamp‖body`, ±5-min skew). So the handler is trusted.

## RegisterCron / RegisterSchedule — timed handlers

Crons (recurring) and schedules (runtime-armed one-shots) share one handler type
`func(ctx, *EventWriter) error`, one `/fire/{slug}` dispatch, and one slug
namespace (unique per agent). Handlers carry **no payload** — they fire by
schedule and survive container suspension, so anything they need (which user,
what reminder) lives in the **agent's own DB**, keyed by the fire id.

```go
agent.RegisterCron(&agentsdk.Cron{
    Slug:     "daily-report",
    Schedule: "0 9 * * *", // standard cron expression
    Handler:  func(ctx context.Context, ew *agentsdk.EventWriter) error { return nil },
    Description: "Generate and send the daily report",
})

agent.RegisterSchedule(&agentsdk.Schedule{
    Slug:    "remind",
    Handler: func(ctx context.Context, ew *agentsdk.EventWriter) error {
        ref, _ := agentsdk.ScheduleFromContext(ctx) // ref.FireID
        // look up the reminder row your tool stored under ref.FireID, then deliver
        return nil
    },
    Description: "Fire one user reminder",
})
```

**Reminder idiom** (per-user, suspension-safe). A tool, not the LLM directly,
drives scheduling — schedules have no built-in user surface:

```go
// in a setReminder tool handler:
u, _ := agentsdk.UserFromContext(ctx)
id, _ := agent.ScheduleAt(ctx, agentsdk.ScheduleAtRequest{Slug: "remind", FireAt: when})
// store (id → u.ID, text) in the agent's own DB
```

On fire, the `remind` handler reads `ScheduleFromContext`, looks up the row, and
delivers via a `PerUser` topic's `PublishToUser(ctx, u.ID, parts)`. Cancel/list
are your own tools over `agent.CancelSchedule(id)` / `agent.ListSchedules(...)`,
scoped against your DB. Never use an in-process timer — the container suspends
and the timer dies.

## RegisterRoute — custom HTTP routes

Routes serve API endpoints or HTML pages directly from the agent, proxied via
the agent's subdomain.

```go
agent.RegisterRoute(&agentsdk.Route{
    Method: "GET",
    Path:   "/api/data",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
    },
    Access:      agentsdk.AccessUser,
    Description: "Get data",
})
```

**Always provide a Description.**

**Access:**
- `AccessUser` — **default choice.** Any agent member.
- `AccessAdmin` — for destructive/sensitive ops (config, delete, reset).
- `AccessPublic` — anyone, no auth. **Only when the user explicitly asks** for
  a public-facing page. Never default to public.

### Framework asset surface — HTML UI

agentsdk bundles htmx and exposes:

- `agentsdk.Assets.HTMX` — versioned URL (e.g. `/__air/assets/htmx-2.0.10.min.js`).
  Use it in your layout `<head>`: `<script src={ agentsdk.Assets.HTMX }></script>`.
- `agentsdk.HTMXVersion` — the bundled version string, if you need it
  programmatically.

**`/__air/assets/*` is framework-reserved.** agentsdk owns this prefix
for its bundled assets. For YOUR own static files (icons, fonts,
page-specific assets), embed them under your agent and serve them from
a route you register — the scaffold uses `/static/{name}` for the
compiled Tailwind stylesheet; extend that handler for additional
files.

> Templ + htmx + Tailwind + DaisyUI build chain, the MVC split,
> view-model conventions, and design taste guidance all live in the
> agent's own **`AGENTS.md`** at the repo root — that's the
> scaffold-level "how to wire this together" doc. This file documents
> only the SDK-side surface.

```go
// Registering a templ page (illustrative — the scaffold already
// wires up `/` to handlers.Home).
import (
    "github.com/a-h/templ"
    "agent/handlers"
)

agent.RegisterRoute(&agentsdk.Route{
    Method:  "GET",
    Path:    "/",
    Handler: handlers.Home,
    Access:  agentsdk.AccessUser,
})
```

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
`conn_{slug}.request(method, path, body?, headers?)` and
`conn_{slug}.requestJSON(...)`. Both return an envelope: small responses
(≤ 8 KiB) come back inline (`body` / `data`); larger ones auto-spill to
`tmp/conn-{slug}-{callID}.bin` with `bodyPreview` + `bodySavedTo` set
(no `body`/`data`). Read the spilled payload with `fileRead(bodySavedTo)` —
see the shared overflow shape in **`/libs/agentsdk/llms/exec.md`**.

## RegisterMCP

```go
github := agent.RegisterMCP(&agentsdk.MCP{
    Slug:     "github",
    Name:     "GitHub",
    URL:      "https://api.githubcopilot.com/mcp",
    AuthMode: agentsdk.MCPAuthOAuthDiscovery, // RFC 9728 auto-discovery
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

**`AuthMode`:** `MCPAuthOAuthDiscovery` (RFC 9728/8414 auto), `MCPAuthOAuth`
(manual URLs), `MCPAuthToken`, `MCPAuthNone`.

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

(Airlock builder: use the `mcp_probe` tool to check what a URL supports before
writing this.)

## RegisterExecEndpoint — remote commands

Declare a slug for running commands on a server the container can't reach as a
built-in tool (a VPS over SSH, a CI runner, a homelab box). Airlock owns the
SSH transport and credentials; the operator configures host/user in the UI; you
wrap calls in typed tools. The handle's `Run(ctx, ExecCommand{...})` returns a
structured small result; `RunStream(...)` streams a data download. Default
`Access` is `AccessAdmin` (`AccessPublic` is silently demoted to `AccessUser`).

```go
ci := agent.RegisterExecEndpoint(&agentsdk.ExecEndpoint{
    Slug:        "ci-runner",
    Description: "Self-hosted GitHub Actions runner",
    LLMHint:     "use `kick-build --branch <name>` to start a build",
    Access:      agentsdk.AccessAdmin,
})
res, err := ci.Run(ctx, agentsdk.ExecCommand{Command: "kick-build", Args: []string{"--branch", "main"}})
```

→ Full API (Run vs RunStream, the `exec_{slug}.run` JS binding, shell features,
errors, and the shared `*SavedTo` overflow handling): **`/libs/agentsdk/llms/exec.md`**.

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
> sealing the session, per-user variant): **`/libs/agentsdk/llms/auth-web.md`**.

## RegisterModel — named model slots

Declare a named slot for every distinct runtime model use case. The admin picks
a specific model per slot in the Airlock UI. At runtime the slug resolves: slot
binding → per-agent default for the slot's capability → system default for that
capability. The slot's `Capability` is the single source of truth for the model
type — the getters take only a slug and read the capability from the slot.

Registration is required: every slug you pass to a model getter must be declared
with `RegisterModel` first. Calling a getter with an unregistered (or empty)
slug panics — a missing declaration is a programmer error, not a silent
fall-through to some default model.

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
`topic_{slug}.subscribe()`.

## RegisterDirectory — file storage

The agent has its own **S3-like object storage** — there is no container
filesystem you expose to tools or the LLM. Every path is a slashless S3 key
(`uploads/x.csv`, `reports/q1.pdf`, `tmp/foo.png`); leading slashes are
rejected. Register a directory to declare per-capability access (`Read` /
`Write` / `List`) and an optional `LLMHint`:

```go
agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessUser, List: agentsdk.AccessUser,
    Description: "User-uploaded source files",
})
```

The **trusted Go file API** (`agent.ReadFile` / `WriteFile` / `OpenFile` /
`StatFile` / `ListDir` / `DeleteFile` / `CopyFile`) bypasses access checks — it's
your code. A path that arrives **from the LLM or any untrusted source** must be
gated first with `agent.CheckFileAccess(ctx, llmPath, agentsdk.OpRead)`.

→ Full directory ACL model, the complete file API, untrusted-path gating,
shelling out to a CLI over storage, and presigned URLs:
**`/libs/agentsdk/llms/files.md`**.

## Agent methods (ctx-first)

Every handler — Tool, Webhook, Cron, Route — receives `context.Context` first.
Pass it through. Model calls and logging are tracked in the Runs UI for the
invoking handler; you never construct a Run yourself.

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

// Storage — trusted; no CheckFileAccess. See /libs/agentsdk/llms/files.md.
agent.OpenFile / ReadFile / WriteFile / StatFile / ListDir / DeleteFile / CopyFile
agent.CheckFileAccess(ctx, llmPath, agentsdk.OpRead) // gate paths from untrusted sources
agent.DB() // *AgentDB — pass to sqlc-generated New() (nil if AIRLOCK_DB_URL unset)
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

## Calling LLMs from agent code (goai)

Crons, webhooks, and tool handlers can call language models via
`agent.LLM(ctx, slug)` and the `goai` package. Calls are proxied through Airlock
so token usage is tracked. Each `slug` must be declared once with
`RegisterModel` — the getter reads the model type from the slot.

**Plain text:**

```go
import (
    "github.com/airlockrun/goai"
    "github.com/airlockrun/goai/message"
    "github.com/airlockrun/goai/stream"
)

func Summarize(ctx context.Context, in SummarizeIn) (SummarizeOut, error) {
    a := agentsdk.AgentFromContext(ctx)
    model := a.LLM(ctx, "summarize")

    result, err := goai.GenerateText(ctx, stream.Input{
        Model: model,
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
    model := a.LLM(ctx, "sentiment")

    result, err := goai.GenerateText(ctx, stream.Input{
        Model: model,
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

## Built-in VM bindings (don't shadow them)

The runtime system prompt fully documents the bindings the LLM can use. The
practical rule for an agent author: **don't name a `RegisterTool` the same as a
built-in or you'll silently lose it.** `RegisterTool` doesn't panic on a
collision — both modes (JS sandbox via `run_js`, direct-tool mode for public
callers) bind built-ins *after* registered tools, so the built-in wins and your
tool becomes unreachable. The behaviour is identical across modes.

Built-in names are **camelCase**. Registered handles auto-bind with a
**`{prefix}_`** name — avoid `RegisterTool` names starting with one of these, or
you shadow your own binding:

| Prefix / name | Source |
|---|---|
| `conn_` | `RegisterConnection` |
| `exec_` | `RegisterExecEndpoint` |
| `topic_` | `RegisterTopic` |
| `mcp_` | `RegisterMCP` |
| `agent_` | A2A sibling tools (synced, not registered by you) |
| `run_js` (reserved) | the JS sandbox entry point |
| `promptAgent` (reserved) | open-ended A2A delegation |

Framework primitives (the runtime prompt describes each in detail).
**Availability**: *all* = every run; *authed* = non-public runs only; *admin* =
admin runs only; *per-dir* = bound only when a registered directory grants the
matching capability:

| Binding(s) | Purpose | Availability |
|---|---|---|
| `fileRead` `fileReadBytes` `fileWrite` `fileStat` `fileList` `fileDelete` `fileExists` `fileShareURL` | object storage; slashless paths; reads cap 16 MiB | per-dir |
| `fileReadRangeBytes` `fileGrep` `fileHead` `fileTail` `fileLines` | large-file reads (exact byte window / line-addressed; cache to local disk) | authed (read verbs also per-dir for public) |
| `fileEncode` `fileDecode` `fileDecodeText` | file→file codec transforms (base64/hex/gzip/charset) | authed |
| `fileEditLines` `fileSed` | streamed line / sed edits | authed |
| `analyzeImage` `transcribeAudio` `generateImage` `speak` `embed` | AI/media (capability-default model) | authed |
| `printToUser` | user-facing output | all |
| `attachToContext` | attach a file to the conversation | authed |
| `httpRequest` `webSearch` | web | authed |
| `log` `console.log/warn/error` | logging | all |
| `queryDB` `execDB` | raw SQL | admin |
| `requestUpgrade` | self-rebuild | admin |

**Public-caller surface is much narrower.** A run triggered by an
`AccessPublic` caller only sees: `printToUser`, `log`/`console`; the file API
verbs only where a registered directory grants `AccessPublic` for the matching
cap (no public dirs → no file API); and the `conn_`/`mcp_`/`topic_`/registered
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
(trusted Go bypasses `CheckFileAccess`), and returns only a presigned
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

// Boot: pull latest in the background so we don't block Serve (no-op on
// first boot when S3 is empty — the image's seeded binary stays).
go func() { _ = agent.SyncDown(context.Background(), "state/bin/", "/var/agent/bin/") }()

// A cron self-updates, then pushes the new binary up.
agent.RegisterCron(&agentsdk.Cron{
    Slug: "bun-refresh", Schedule: "0 3 * * 0",
    Handler: func(ctx context.Context, _ *agentsdk.EventWriter) error {
        if err := exec.CommandContext(ctx, "/var/agent/bin/bun", "upgrade").Run(); err != nil {
            return err
        }
        return agent.SyncUp(ctx, "/var/agent/bin/", "state/bin/")
    },
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

**Health-check budget:** the platform expects `agent.Serve()` to start within
~15 seconds of container start. Anything slower than a few hundred ms —
`SyncDown`, third-party warm-up, model preload — must run in a goroutine *after*
you call `Serve`, not synchronously in `main()` before it.

---

# Database access

A full Postgres schema (usually with pgvector — pair vector columns with
`agent.EmbeddingModel(ctx, slug)`). Tables via goose migrations in
`db/migrations/`; queries via sqlc (`db/queries/` → generated `internal/db/`).
`agent.DB()` returns `*AgentDB` (wraps `*sql.DB`, `nil` if `AIRLOCK_DB_URL`
unset) — pass it straight to the generated `New()`. Migrations run automatically
at container startup. **Always use sqlc** — never raw `db.QueryRow` / `db.Exec`
strings in Go.

```go
db := agent.DB()
queries := internaldb.New(db) // import "agent/internal/db" as internaldb
users, err := queries.ListActiveUsers(ctx)
```

→ Migration file format, numbering, Go migrations (Tx vs NoTx), guarding
external side effects with `IsValidatingMigrations()`, and build-time
validation: **`/libs/agentsdk/llms/database.md`**.
