# Building Airlock agents with agentsdk

This is the reference for writing an Airlock agent in Go with `agentsdk`. It
documents the SDK surface and the patterns that make an agent behave well at
runtime. It is consumed two ways, and both should treat it as authoritative:

- **The Airlock agent-builder** reads it before generating or upgrading agent
  code.
- **You, by hand** — point your editor's AI at this file (it ships in the
  `agentsdk` module) or read it directly when writing or modifying an agent.

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

## Project layout

```
{agent-repo}/
├── main.go            # Entrypoint — registrations only, no business logic
├── go.mod             # Module file (usually leave as-is)
├── sqlc.yaml          # sqlc config (pre-configured)
├── setup.sh           # (optional) system setup — runs as root at image build, baked into runtime image
├── views/
│   ├── layout.templ
│   └── index.templ
├── db/
│   ├── migrations/    # goose SQL/Go migrations (you create)
│   │   └── doc.go     # package declaration (pre-scaffolded)
│   └── queries/       # sqlc query files (you create)
└── internal/db/       # sqlc-generated code
```

**Keep `main.go` thin** — registrations only. Business logic lives in domain
packages:

```
{agent-repo}/
├── main.go         # Registrations
├── deps/
│   └── deps.go     # Deps struct — shared by all domain packages
├── spotify/
│   ├── service.go  # Pure Go (handles + plain types)
│   └── tools.go    # Tool Execute funcs
```

`Deps` is a single struct shared by every domain package, so it lives in
its own `deps` package — not inside a feature package. A feature package
(`spotify`) can't be imported by a sibling (`weather`) without coupling
them, and `main` can't be imported at all. Keeping `Deps` in `deps/`
lets `main.go` and every domain package import the same type.

Each domain package has two layers:

1. **Pure Go** — business logic, API calls. Accept handles, return plain types.
2. **Tool Execute** — `func(ctx, In) (Out, error)` that pulls handles via
   `GetDeps` and calls into pure Go.

## Minimal example — Weather agent

```go
// main.go
package main

import (
    "context"
    "io"
    "net/http"

    "github.com/airlockrun/agentsdk"
)

func main() {
    agent := agentsdk.New(agentsdk.Config{
        Description: "Weather agent — current conditions for any city",
    })

    type WeatherIn struct {
        City string `json:"city" jsonschema:"description=City name, e.g. London"`
    }
    type WeatherOut struct {
        Raw string `json:"raw"`
    }
    agent.RegisterTool(&agentsdk.Tool[WeatherIn, WeatherOut]{
        Name:        "get_weather",
        Description: "Get current weather for a city.",
        Execute: func(ctx context.Context, in WeatherIn) (WeatherOut, error) {
            resp, err := http.Get("https://wttr.in/" + in.City + "?format=j1")
            if err != nil {
                return WeatherOut{}, err
            }
            defer resp.Body.Close()
            body, _ := io.ReadAll(resp.Body)
            return WeatherOut{Raw: string(body)}, nil
        },
        Access: agentsdk.AccessUser,
    })

    agent.Serve()
}
```

The LLM can now call `get_weather({city: "London"})` in `run_js`.

## Design principle: always register granular tools

Whenever you build a feature (route, cron, webhook, connection), also register
**granular tools** giving the LLM the same data and operations.

Bad: only `importPlaylist` (bulk insert). The LLM can import but can't inspect
or query.

Good: `importPlaylist`, `listSongs`, `getSong`, `voteSong`. Now the LLM can
answer "which song has the most votes?" through `run_js`.

Think: "what would the LLM need to call to be a helpful conversational
assistant in this domain?" and register those tools.

## Worked example — Spotify agent

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

type Track struct {
    Name, URI, Artist string
}

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

type PlayIn  struct { TrackURI string `json:"trackURI,omitempty"` }
type PlayOut struct { OK bool `json:"ok"` }

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
- `RegisterConnection` returns `*ConnectionHandle`; use it for all API calls
- `agent.Deps` stores handles; tool funcs retrieve via `agentsdk.GetDeps[*deps.Deps](ctx)`
- `handle.Request(ctx, agentsdk.RequestOpts{Path: ...})` returns raw
  bytes. `RequestOpts.Method` defaults to `"GET"`; `Body` auto-encodes
  (struct → JSON, `[]byte`/`string` as-is, `nil` → no body); `Headers`
  is an optional `map[string]string` for per-call request headers.
  Airlock injects credentials.
- `LLMHint` is appended to the connection block in the runtime system prompt

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

## AddExtraPrompt — access-scoped system prompt fragments

Airlock already renders a base system prompt covering `run_js`, registered
tools (with TS signatures), MCP tools, connection helpers, public-caller safety
guards, and environment context (date, platform, conversation id). **Do not
re-declare any of that.** `AddExtraPrompt` is only for content the baseline
can't infer:

- The agent's persona, tone, voice
- Domain rules the LLM can't deduce from the tool signatures
- Per-access behavior differences the user explicitly asked for

```go
agent.AddExtraPrompt(&agentsdk.ExtraPrompt{
    Text: "You are a concise events assistant for the Berlin meetup. Answer in English.",
})
agent.AddExtraPrompt(&agentsdk.ExtraPrompt{
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
    Verify:      "hmac",                // "hmac" | "token" | "none" (default "none")
    Header:      "X-Hub-Signature-256", // header for verification
    Description: "GitHub push events",
    Access:      agentsdk.AccessUser,
})
```

## RegisterCron

```go
agent.RegisterCron(&agentsdk.Cron{
    Name:     "daily-report",
    Schedule: "0 9 * * *",
    Handler: func(ctx context.Context, ew *agentsdk.EventWriter) error {
        return nil
    },
    Description: "Generate and send daily report",
})
```

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

### templ + htmx — HTML UI

Agents use [templ](https://templ.guide) for type-safe HTML and
[htmx](https://htmx.org) for interactivity. The scaffold has `views/` with a
layout and index. htmx is loaded from CDN.

**Before any Go command (`go build`, `go vet`, `go test`, ...), run
`go tool templ generate` first.** The scaffold contains only `.templ` source;
without generation, Go reports misleading errors like "package agent/views is
not in std". Re-run whenever you modify a `.templ`. The Docker build re-runs
it; you don't commit generated files.

```go
// Register a templ page
import (
    "github.com/a-h/templ"
    "agent/views"
)

agent.RegisterRoute(&agentsdk.Route{
    Method: "GET", Path: "/",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        templ.Handler(views.Index()).ServeHTTP(w, r)
    },
    Access: agentsdk.AccessPublic, Description: "Homepage",
})
```

```
// views/search.templ
package views

templ SearchForm() {
    <form hx-get="/search" hx-target="#results">
        <input type="text" name="q"/>
        <button type="submit">Search</button>
    </form>
    <div id="results"></div>
}

templ SearchResults(items []string) {
    <ul>
        for _, item := range items {
            <li>{ item }</li>
        }
    </ul>
}
```

Returning a partial:

```go
agent.RegisterRoute(&agentsdk.Route{
    Method: "GET", Path: "/search",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        results := doSearch(ctx, r.URL.Query().Get("q"))
        views.SearchResults(results).Render(ctx, w)
    },
    Access: agentsdk.AccessUser, Description: "Search results partial",
})
```

Full pages wrap content in `@Layout("Title") { ... }`; htmx requests get
partial HTML (no layout).

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
(no `body`/`data`). See **Reading overflowed responses** under
`RegisterExecEndpoint`.

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

## RegisterExecEndpoint — run commands on a remote target

Use when the agent needs to run a command on a server its container can't
reach as a built-in tool (managing a VPS via SSH, kicking a CI runner,
restarting a service on a homelab box). Airlock owns the transport
(SSH today) and the credentials; the agent author declares the slug and
wraps calls in domain-specific tools.

```go
ci := agent.RegisterExecEndpoint(&agentsdk.ExecEndpoint{
    Slug:        "ci-runner",
    Description: "Self-hosted GitHub Actions runner",
    LLMHint:     "use `kick-build --branch <name>` to start a build",
    Access:      agentsdk.AccessAdmin, // default; opt down explicitly
})

type KickIn struct {
    Branch string `json:"branch" jsonschema:"description=Branch to build"`
}
type KickOut struct {
    ExitCode int    `json:"exitCode"`
    Stdout   string `json:"stdout"`
}

agent.RegisterTool(&agentsdk.Tool[KickIn, KickOut]{
    Name:        "kick_build",
    Description: "Kick the CI build for a branch.",
    Access:      agentsdk.AccessAdmin,
    Execute: func(ctx context.Context, in KickIn) (KickOut, error) {
        res, err := ci.Run(ctx, agentsdk.ExecCommand{
            Command: "kick-build",
            Args:    []string{"--branch", in.Branch},
            Timeout: 30 * time.Second,
        })
        if err != nil {
            return KickOut{}, err
        }
        return KickOut{ExitCode: res.ExitCode, Stdout: string(res.Stdout)}, nil
    },
})
```

**Configuration is operator-side.** The agent declares the slug,
description, hint, and access level. The operator configures
host/port/user in the Airlock UI; airlock generates an ED25519 keypair,
displays the public key, and the operator pastes it into
`~/.ssh/authorized_keys` on the target. Private keys never leave
airlock and are encrypted at rest. The public key carries a dated
comment (`airlock-{agentSlug}-{endpointSlug}-YYYY-MM-DD`) so on rotation
the operator can `grep` `authorized_keys` and remove old lines.

**Default access is `AccessAdmin`.** Exec hands arbitrary commands to a
real machine; admin-only is the right default. **`AccessPublic` is
silently demoted to `AccessUser`** at registration time with a startup
warning — exec endpoints are never reachable by unauthenticated callers,
period. (The demotion is a friendly recovery, not an error, because
copy-pasting from `RegisterRoute` where Public is meaningful is a
believable mistake.)

**`ExecHandle.Run` is bound into the JS VM as `exec_{slug}.run(command, args?, opts?)`** —
gated at bind time on `Access`. Wrap in typed tools when you want a
narrower verb than "run an arbitrary command":

```javascript
// JS-side (run_js); only available on admin runs by default
exec_ci.run("kick-build", ["--branch", "main"])
// inline (both streams ≤ 8 KiB):
//   { exitCode, durationMs, stdout: "…", stderr: "…" }
// spilled (stdout exceeded 8 KiB):
//   { exitCode, durationMs,
//     stdoutPreview: "…1 KiB head…",
//     stdoutSavedTo: "tmp/exec-ci-a3f9c2b1-stdout.bin",
//     stdoutSize: 50000,
//     stderr: "…",
//     note: "stdout (50000 bytes) exceeded inline threshold; saved to … Use fileRead(stdoutSavedTo) to read." }
```

When `stdoutSavedTo` or `stderrSavedTo` is set, the corresponding
`stdout`/`stderr` field is absent — read the full payload with
`fileRead(savedTo)`. See **Reading overflowed responses** below.

**Shell features (pipes, redirection, env expansion) work** because SSH
hands the full command line to the remote shell. Put pipes in
`Command`; use `Args` for safe multi-arg invocation (args are
POSIX-shell-quoted before being joined onto the remote command):

```go
// Multi-arg with whitespace — args are quoted safely.
vps.Run(ctx, agentsdk.ExecCommand{
    Command: "ls",
    Args:    []string{"-la", "my dir"},
})
// Pipe + JSON parse — everything in Command, no Args.
vps.Run(ctx, agentsdk.ExecCommand{
    Command: "kubectl get pods -o json | jq '.items[] | .metadata.name'",
})
```

**Errors:** `*agentsdk.ExecError` for transport / timeout / config
problems (the command never ran — different retry strategy than a
runtime failure). Non-zero exit codes return a normal `ExecResult` —
inspect `ExitCode` and `Stderr`.

**`Run` is for structured small responses** (JSON API replies, HTML
pages, CLI summaries that fit in your head). Cap is **20 MiB per
stream**; overflow returns `agentsdk.ErrOutputTooLarge` with no
partial result. The run-record audit log keeps an 8 KiB preview per
stream for visibility in the runs UI; the caller of `Run` still
receives the full data up to the 20 MiB cap.

**`RunStream` is the default for any actual data download.** Tarballs,
log archives, database dumps — anything you'd describe as "data"
rather than "a result." Returns an `*ExecStream` whose `Stdout`/`Stderr`
are `io.ReadCloser`s and `Wait()` blocks for the exit code:

```go
s, err := vps.RunStream(ctx, agentsdk.ExecCommand{
    Command: "tar -czf - /var/log",
})
if err != nil { return err }
defer s.Stdout.Close()
defer s.Stderr.Close()

// Pipe straight into agent storage — zero buffering in agent RAM.
info, _ := agent.WriteFile(ctx, "tmp/logs.tar.gz", s.Stdout, "application/gzip")
exit, _ := s.Wait()
if exit.ExitCode != 0 {
    stderrBytes, _ := io.ReadAll(s.Stderr)
    return fmt.Errorf("tar failed: %s", stderrBytes)
}
return nil
```

`RunStream` is **Go-only** — there is no JS binding for it. The
JS-facing `exec_{slug}.run` streams under the hood and auto-spills
stdout/stderr above 8 KiB to `tmp/exec-{slug}-{callID}-stdout.bin` /
`-stderr.bin` (both halves of a call share `callID` so they correlate).

**Rule of thumb:** if the downstream code is going to call `JSON.parse`
on the result, use `Run`. If it's going to call `agent.WriteFile`, use
`RunStream` and skip the round-trip through agent RAM.

### Reading overflowed responses

`conn_{slug}.request`, `conn_{slug}.requestJSON`, `exec_{slug}.run`, and
`httpRequest` all share the same overflow shape: any payload above 8 KiB
is saved to a `tmp/...` storage path, with `*Preview` holding the first
~1 KiB and `*SavedTo` holding the path. Inline and spilled keys are
mutually exclusive — check the saved-to field and branch:

```javascript
// Connection
const r = conn_x.request("GET", "/big")
const body = r.bodySavedTo ? fileRead(r.bodySavedTo) : r.body

// Connection (JSON)
const r = conn_x.requestJSON("GET", "/big.json")
const data = r.bodySavedTo ? JSON.parse(fileRead(r.bodySavedTo)) : r.data

// Exec
const r = exec_x.run("dump-state")
const stdout = r.stdoutSavedTo ? fileRead(r.stdoutSavedTo) : r.stdout
```

Don't re-read an older `savedTo` after a fresh call to the same binding
— each call gets a unique `callID` so paths don't collide within a run,
but holding onto a stale path across multiple LLM steps is a bug source
the `note` field warns about.

## RegisterEnvVar — operator-configured environment variables

**Use sparingly.** For ordinary configuration, just define values in code —
env vars add operator burden for no benefit. For credentials that authenticate
proxied HTTP/MCP calls, prefer `RegisterConnection` / `RegisterMCP` with
`AuthInjection` — Airlock injects the credential at proxy time and the agent
code never touches it.

`RegisterEnvVar` is for the cases those don't cover:
- The user explicitly asked for a configurable env var.
- You're shelling out to a CLI that reads its credentials from environment
  variables and there's no way to inject them server-side.

Two flavours, controlled by the `Secret` flag:

```go
// Plain config — operator sees and edits the current value in the UI.
// Default lets the agent ship with a working setting; operator only
// overrides when needed.
region := agent.RegisterEnvVar(&agentsdk.EnvVar{
    Slug:        "aws_region",
    Description: "Default AWS region",
    Default:     "us-east-1",
    Pattern:     `^[a-z]{2}-[a-z]+-\d$`, // optional regex; rejected on save if no match
})

// Secret — write-only in the UI (no read-back, only rotate). Auto-added
// to the redact set on first Get(). Default is forbidden for secrets.
accessKey := agent.RegisterEnvVar(&agentsdk.EnvVar{
    Slug:        "aws_access_key_id",
    Description: "AWS IAM access key id",
    Secret:      true,
    Pattern:     `^AKIA[0-9A-Z]{16}$`,
})
secretKey := agent.RegisterEnvVar(&agentsdk.EnvVar{
    Slug:        "aws_secret_access_key",
    Description: "AWS IAM secret access key",
    Secret:      true,
    Pattern:     `^.+$`, // require non-empty; no specific shape known
})
```

Subprocess CLI example — an `s3_list` tool that wraps `aws s3 ls`:

```go
agent.RegisterTool(&agentsdk.Tool[S3ListIn, S3ListOut]{
    Name: "s3_list",
    Execute: func(ctx context.Context, in S3ListIn) (S3ListOut, error) {
        // Pattern is declared on every credential here, so Get's error
        // doubles as the "not configured" check — no separate empty-string
        // guard needed. Surface the slug so the operator knows what to set.
        ak, err := accessKey.Get(ctx)
        if err != nil {
            return S3ListOut{}, fmt.Errorf("set aws_access_key_id in the agent's Environment tab: %w", err)
        }
        sk, err := secretKey.Get(ctx)
        if err != nil {
            return S3ListOut{}, fmt.Errorf("set aws_secret_access_key in the agent's Environment tab: %w", err)
        }
        rg, err := region.Get(ctx)
        if err != nil { return S3ListOut{}, err }

        cmd := exec.CommandContext(ctx, "aws", "s3", "ls", in.Bucket)
        cmd.Env = append(os.Environ(),
            "AWS_ACCESS_KEY_ID="+ak,
            "AWS_SECRET_ACCESS_KEY="+sk,
            "AWS_DEFAULT_REGION="+rg,
        )
        out, err := cmd.CombinedOutput()
        return S3ListOut{Output: string(out)}, err
    },
})
```

**Get's contract**: returns the stored value (or `Default`, or `""` if
neither). The error is non-nil on transport/decrypt failure, **and** when the
fetched value doesn't match the declared `Pattern` — including the empty
string. So if you declare `Pattern: "^.+$"` (or any tighter regex), `err != nil`
is exactly your "operator hasn't configured this yet" signal; no separate
`if v == ""` guard is needed. With no `Pattern`, `("", nil)` is a valid
successful return and the agent code is responsible for deciding what to do
with it.

**Pattern** is an optional regex — Airlock rejects operator-supplied values
that don't match at save time, *and* `Get` re-checks at fetch time. Use it for
credentials with a known shape (AWS keys, region codes) so typos surface
immediately, or use `^.+$` to require non-empty.

`Default` is forbidden when `Secret: true` (the SDK panics) — secrets must come
from the operator.

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

### Auth flows that need user input → an admin web page

Many credentials can't be minted headlessly: the login emits a one-time code,
asks for a password, or needs a click. That input is **ephemeral and
interactive** — it can't be an env var. Drive it from an `AccessAdmin`
`RegisterRoute` page (templ + htmx): an admin performs the interactive step, the
agent finishes the login, and you `Seal` the resulting long-lived credential.
Later runs `Unseal` it and never prompt again.

**Worked example — a messaging CLI (`msgcli`) the agent shells out to.** It
needs API credentials (env) *and* an interactive phone-code login (admin page),
and it authenticates with a session string we seal:

```go
// Config the binary reads from its environment. api_hash is a credential.
var (
    apiID = agent.RegisterEnvVar(&agentsdk.EnvVar{
        Slug: "msg_api_id", Description: "API ID from the provider console",
        Pattern: `^[0-9]+$`,
    })
    apiHash = agent.RegisterEnvVar(&agentsdk.EnvVar{
        Slug: "msg_api_hash", Description: "API hash from the provider console",
        Secret: true, Pattern: `^[a-f0-9]{32}$`,
    })
)

const sessionPath = "state/msg-session.enc" // sealed blob, agent-wide

// runCLI shells out with env + the unsealed session (when one exists).
func runCLI(ctx context.Context, args ...string) ([]byte, error) {
    id, err := apiID.Get(ctx)
    if err != nil {
        return nil, fmt.Errorf("set msg_api_id in the Environment tab: %w", err)
    }
    hash, err := apiHash.Get(ctx)
    if err != nil {
        return nil, fmt.Errorf("set msg_api_hash in the Environment tab: %w", err)
    }
    env := append(os.Environ(), "MSG_API_ID="+id, "MSG_API_HASH="+hash)

    if blob, err := agent.ReadFile(ctx, sessionPath); err == nil {
        session, err := agent.Unseal(ctx, string(blob))
        if err != nil {
            return nil, fmt.Errorf("stored session unreadable — re-run login: %w", err)
        }
        env = append(env, "MSG_SESSION="+session)
    }
    cmd := exec.CommandContext(ctx, "msgcli", args...)
    cmd.Env = env
    return cmd.CombinedOutput()
}

// --- admin login page: the only place the plaintext session exists ---

// GET /admin/login — render the phone form.
agent.RegisterRoute(&agentsdk.Route{
    Method: "GET", Path: "/admin/login", Access: agentsdk.AccessAdmin,
    Description: "Interactive login page for the messaging account",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        templ.Handler(views.PhoneForm()).ServeHTTP(w, r)
    },
})

// POST /admin/login/start — CLI sends a one-time code to the operator's device.
agent.RegisterRoute(&agentsdk.Route{
    Method: "POST", Path: "/admin/login/start", Access: agentsdk.AccessAdmin,
    Description: "Begin login: trigger the one-time code",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        phone := r.FormValue("phone")
        if _, err := runCLI(ctx, "login", "start", "--phone", phone); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        views.CodeForm(phone).Render(ctx, w) // htmx swaps in the code form
    },
})

// POST /admin/login/verify — finish login, seal the session, persist it.
agent.RegisterRoute(&agentsdk.Route{
    Method: "POST", Path: "/admin/login/verify", Access: agentsdk.AccessAdmin,
    Description: "Finish login and store the sealed session",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        out, err := runCLI(ctx, "login", "verify",
            "--phone", r.FormValue("phone"), "--code", r.FormValue("code"))
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        session := strings.TrimSpace(string(out)) // the long-lived session string

        sealed, err := agent.Seal(ctx, session)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if _, err := agent.WriteFile(ctx, sessionPath, strings.NewReader(sealed), "text/plain"); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        views.LoginDone().Render(ctx, w)
    },
})

// The LLM-facing tool just runs the binary; the session is wired in by runCLI.
agent.RegisterTool(&agentsdk.Tool[SendIn, SendOut]{
    Name:        "send_message",
    Description: "Send a message via the linked account.",
    Execute: func(ctx context.Context, in SendIn) (SendOut, error) {
        out, err := runCLI(ctx, "send", "--to", in.To, "--text", in.Text)
        if err != nil {
            return SendOut{}, fmt.Errorf("not linked yet — an admin must complete login at the agent's /admin/login page: %w", err)
        }
        return SendOut{Result: string(out)}, nil
    },
})
```

For **per-user** login — the SaaS-style case where the agent's own end users
each link their account through public signup/login pages the agent serves —
Airlock supplies no caller identity to lean on. The agent runs its own auth on
`AccessPublic` routes, keeps a `users` table and sessions in its own Postgres
schema, and keys each sealed session by *its* `user_id`. `runCLI` resolves the
current user from the agent's session (cookie / token / however its pages
identify the request) and loads that row instead of `ReadFile`. The
seal/unseal calls are identical — only where you keep the ciphertext, and how
you identify whose it is, changes.

## RegisterModel — named model slots

Declare a named slot for every distinct runtime LLM use case. The admin picks a
specific model per slot in the Airlock UI. At runtime the slug resolves: slot
binding → per-agent capability override → system default. Undeclared slugs
still work (fall through to the capability default), but they're not listed in
the admin UI, so you can't rebind them.

```go
agent.RegisterModel(&agentsdk.ModelSlot{
    Slug:        "summarize",
    Capability:  agentsdk.CapText,
    Description: "Short summaries for weekly reports",
})

model := agent.LLM(ctx, "summarize", agentsdk.ModelOpts{})
```

**Capabilities:**
- `CapText` / `CapVision` → `agent.LLM(ctx, slug, ModelOpts{Capability: ...})`
- `CapImage` → `agent.ImageModel(ctx, slug, ModelOpts{})`
- `CapSpeech` → `agent.SpeechModel(ctx, slug, ModelOpts{})` (TTS)
- `CapTranscription` → `agent.TranscriptionModel(ctx, slug, ModelOpts{})` (STT)
- `CapEmbedding` → `agent.EmbeddingModel(ctx, slug, ModelOpts{})`

The built-in VM media helpers use empty slugs intentionally — they resolve via
the per-agent capability default.

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

The agent has its own **S3-like object storage**. There is no container
filesystem you expose to tools or to the LLM — every path you read or write is
an S3 key. Paths are slashless: `uploads/x.csv`, `reports/q1.pdf`,
`tmp/foo.png`. Leading slashes are rejected by the SDK (the LLM and your Go
code share one canonical form).

**Hard rule for tool authors: tool inputs and outputs are *storage* paths,
never container paths.** A `Source agentsdk.FilePath` field on a tool's `In`
struct is a storage path the LLM (or a sibling agent over A2A) passed in;
convert with `string(in.Source)` and pass it through `CheckFileAccess` /
`agent.OpenFile`. A `Result agentsdk.FilePath` you return is a storage path
the LLM, chat, or calling sibling will follow back through the same storage
namespace. Returning `os.CreateTemp` paths or `localOut.Name()` to the LLM
gives it a path it cannot read — the framework will 404. When you need a real
on-disk file (CLI tools like `ffmpeg`, `pdftotext`), use `os.CreateTemp`
purely as scratch inside the tool body, then upload the result with
`agent.WriteFile` and return the resulting path as `agentsdk.FilePath`.

```go
agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessUser, List: agentsdk.AccessUser,
    Description: "Files the user uploaded in chat.",
})
agent.RegisterDirectory("reports", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessAdmin, List: agentsdk.AccessUser,
    Description: "Generated reports.",
})
agent.RegisterDirectory("cache", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Description: "Memoized responses.",
    LLMHint:     "internal cache; do not read, write, or list",
})
agent.RegisterDirectory("generated-images", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Description:    "Throwaway AI-generated images served via fileShareURL.",
    LLMHint:        "served externally via fileShareURL only; do not read or list directly",
    RetentionHours: 24, // sweeper drops files older than 24h
})
```

`Read` / `Write` / `List` are independent caps. `delete` folds into `Write`
(write on the parent governs unlink). Each can be `AccessAdmin`, `AccessUser`,
or `AccessPublic`. To hide a directory from the LLM while keeping it reachable
from your Go code, set the caps to `AccessAdmin` and add an `LLMHint` like
`"internal cache; do not read, write, or list"` — the hint is purely
model-facing guidance surfaced in the system prompt, while the trusted Go file
API (`agent.OpenFile`/`ReadFile`/...) bypasses `CheckFileAccess` entirely so
your code can use the directory freely.

**`RetentionHours`** opts the directory into Airlock's storage sweeper: any
object older than the configured hours is deleted on the ~6h sweep tick.
Default `0` means files live forever — that's right for `uploads`, `reports`,
anything the user expects to find tomorrow. Set a TTL on directories that hold
ephemeral artifacts (generated media, transient scratch, presigned-URL
targets) so the bucket doesn't grow without bound. Match the TTL to whatever
URL expiry you hand out: a `fileShareURL(path, {expiresInMinutes: 60})` link is
useless after an hour, so the bytes can go away on roughly the same horizon.

**`Scope`** — **use rarely.** Default (`ScopeNone`) is correct for almost every
directory; reach for scoping only when the user explicitly asks for per-user
(or per-conversation) file isolation, or when a tool that produces files must
be exposed at the public tier. Don't scope `uploads`, `reports`, `cache`, or
general working directories — that breaks the natural "the agent and its
members share these files" model and surprises the user when files "disappear"
across conversations.

When you do need it: `Scope` opts the directory into per-context path
isolation. WriteFile inserts a scope segment (`user-<id>` / `conv-<id>` /
`run-<id>`) under the directory prefix; CheckFileAccess accepts the matching
segment on reads. The author's code is unchanged —
`agent.WriteFile(ctx, "gen/cat.jpg", ...)` returns the scoped path, the LLM
passes it around, and reads just work for the caller who wrote it. Use this
instead of opening a directory to `AccessPublic` when a public-tier tool
produces files: `Scope: ScopeUser` (with `Read/Write/List=AccessAdmin`) keeps
the base ACL locked while letting each caller see their own slot. `ScopeUser`
falls back to conv → run when no logged-in user is available, so anon callers
still get isolation.

```go
agent.RegisterDirectory("gen", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Scope:          agentsdk.ScopeUser,
    Description:    "Per-caller generated artifacts.",
    RetentionHours: 24,
})
// tool body:
out, _ := agent.WriteFile(ctx, "gen/result.png", buf, "image/png")
// out.Path is "gen/user-<uuid>/result.png" — return that to the LLM.
```

The framework auto-registers `tmp` at `Read=Write=List=AccessUser` with
`RetentionHours: 72` (unscoped — accessible to any user-tier caller). You may
`RegisterDirectory("tmp", DirectoryOpts{Description: "..."})` to customize the
description; the access caps, retention, and unscoped behaviour stay at the
framework's values.

### Trusted Go file API — for code that constructs paths

```go
src,  err := agent.OpenFile(ctx, "uploads/doc.pdf")          // io.ReadCloser
data, err := agent.ReadFile(ctx, "uploads/notes.txt")        // []byte
info, err := agent.WriteFile(ctx, "reports/q1.csv", reader, "text/csv")
info, err := agent.StatFile(ctx, "uploads/doc.pdf")          // FileInfo
files, err := agent.ListDir(ctx, "uploads/", agentsdk.ListOpts{Recursive: false})
err := agent.DeleteFile(ctx, "reports/old.csv")              // idempotent
err := agent.CopyFile(ctx, "uploads/in.csv", "reports/copy.csv")
share, err := agent.ShareFileURL(ctx, "reports/q1.csv", time.Hour) // {URL, ExpiresAtMs}
```

These do **not** call `CheckFileAccess` — agent Go is trusted with paths it
constructs itself. Use freely from cron/webhook handlers, internal caches,
anywhere.

`ShareFileURL` returns a presigned, unauthenticated, time-limited URL (default
1h, capped at 24h server-side). Use it from cron/webhook handlers when you need
to email/post a one-off external link to a stored file. For chat delivery,
prefer `printToUser({type:"file", source:path})` from `run_js`; for member-only
access on a stable URL, the agent's `__air/storage/{path}` route already
handles it.

### Untrusted paths — when a tool accepts a path from the LLM

If a tool input declares a path field, the path is **untrusted**. The LLM can
pass any string, including paths under internal directories or with `..`
segments. **Call `agent.CheckFileAccess` before using the path anywhere** —
even before passing it to an external API.

```go
type CropIn struct {
    Image   agentsdk.FilePath `json:"image"`
    X, Y, W, H int
}
type CropOut struct {
    Result agentsdk.FilePath `json:"result"`
}

agent.RegisterTool(&agentsdk.Tool[CropIn, CropOut]{
    Name:        "crop_image",
    Description: "Crop a stored image and save the result.",
    Access:      agentsdk.AccessUser,
    Execute: func(ctx context.Context, in CropIn) (CropOut, error) {
        src := string(in.Image) // FilePath → string for the file API
        if err := agent.CheckFileAccess(ctx, src, agentsdk.OpRead); err != nil {
            return CropOut{}, err
        }
        r, err := agent.OpenFile(ctx, src)
        if err != nil {
            return CropOut{}, err
        }
        defer r.Close()
        cropped := crop(r, in.X, in.Y, in.W, in.H)
        info, err := agent.WriteFile(ctx, "tmp/crop-"+randHex(4)+".png",
            bytes.NewReader(cropped), "image/png")
        if err != nil {
            return CropOut{}, err
        }
        return CropOut{Result: info.Path}, nil
    },
})
```

`CheckFileAccess` returns `agentsdk.ErrNotFound` for both "denied" and "no
covering directory" — the indistinguishable response prevents path-guessing
from leaking what exists.

### Shelling out to a CLI tool

CLI tools (ffmpeg, pdftotext, imagemagick, libreoffice, …) need real on-disk
files. The shape is always: permission-check the LLM-supplied storage path,
download to a temp file, shell out, upload the result back to storage, clean
up. Temp files are scratch internal to the tool — they never appear in inputs
or outputs.

```go
type TranscodeIn struct {
    Source agentsdk.FilePath `json:"source"`
}
type TranscodeOut struct {
    Result agentsdk.FilePath `json:"result"`
}

Execute: func(ctx context.Context, in TranscodeIn) (TranscodeOut, error) {
    srcPath := string(in.Source) // FilePath → string for the file API
    if err := agent.CheckFileAccess(ctx, srcPath, agentsdk.OpRead); err != nil {
        return TranscodeOut{}, err
    }

    // Download the storage object into a scratch temp file.
    src, err := agent.OpenFile(ctx, srcPath)
    if err != nil {
        return TranscodeOut{}, err
    }
    defer src.Close()
    inFile, err := os.CreateTemp("", "in-*"+filepath.Ext(srcPath))
    if err != nil {
        return TranscodeOut{}, err
    }
    defer os.Remove(inFile.Name())
    if _, err := io.Copy(inFile, src); err != nil {
        inFile.Close()
        return TranscodeOut{}, err
    }
    inFile.Close()

    // Run the CLI against the scratch files.
    outFile, _ := os.CreateTemp("", "out-*.mp3")
    outFile.Close()
    defer os.Remove(outFile.Name())
    cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inFile.Name(),
        "-b:a", "128k", outFile.Name())
    if out, err := cmd.CombinedOutput(); err != nil {
        return TranscodeOut{}, fmt.Errorf("ffmpeg: %w: %s", err, out)
    }

    // Upload the result back into storage and return the path as FilePath
    // so airlock auto-copies it to the caller across A2A boundaries.
    result, err := os.Open(outFile.Name())
    if err != nil {
        return TranscodeOut{}, err
    }
    defer result.Close()
    info, err := agent.WriteFile(ctx, "transcoded/"+filepath.Base(outFile.Name()),
        result, "audio/mpeg")
    if err != nil {
        return TranscodeOut{}, err
    }
    return TranscodeOut{Result: info.Path}, nil
},
```

Skipping any step either leaks across the security boundary or leaves the
result invisible to the rest of the agent:

- The two `defer os.Remove` calls fire on every exit path; without them the
  container's scratch disk fills up over time.
- Never return `outFile.Name()` (or any `os.CreateTemp` path) to the LLM. That
  path doesn't exist in the agent's storage; `agent.StatFile` would 404. Always
  upload with `agent.WriteFile` and return the resulting path as
  `agentsdk.FilePath`.
- For tools whose output filename you don't control, point the CLI at a temp
  *directory* (`os.MkdirTemp`), `defer os.RemoveAll` it, then walk and upload.

> **Use ctx-aware primitives in tool Execute.** The `ctx` in `Execute(ctx, in)`
> cancels when the user cancels or the run timeout fires.
> `exec.CommandContext`, `http.NewRequestWithContext`,
> `pgx.Pool().Query(ctx, ...)`, and
> `select { case <-ctx.Done(): ...; case <-time.After(d): }` honor it;
> `time.Sleep`, `io.ReadAll` without ctx, and blocking syscalls don't. A
> non-cooperative tool keeps running until it returns naturally — the run row
> finalizes correctly, but cancellation feels broken to the user.

### Public URLs for storage

Every registered directory is reachable at
`https://{slug}.{agentDomain}/__air/storage/{path}`; the proxy enforces the
directory's `Read` cap at fetch time:

- `AccessPublic` — unauthenticated, anyone with the URL.
- `AccessUser` — requires agent membership (proxy redirects through
  relay-login).
- `AccessAdmin` — same, admin role.

## Agent methods (ctx-first)

Every handler — Tool, Webhook, Cron, Route — receives `context.Context` first.
Pass it through. Model calls and logging are tracked in the Runs UI for the
invoking handler; you never construct a Run yourself.

```go
// Models — all ctx-first
agent.LLM(ctx, slug, agentsdk.ModelOpts{})                 // streaming language model
agent.ImageModel(ctx, slug, agentsdk.ModelOpts{})
agent.SpeechModel(ctx, slug, agentsdk.ModelOpts{})         // TTS
agent.TranscriptionModel(ctx, slug, agentsdk.ModelOpts{})  // STT
agent.EmbeddingModel(ctx, slug, agentsdk.ModelOpts{})

// Logging — agent.Logger(ctx) returns a *zap.Logger. Bind it once at
// handler entry; the ctx is consumed there to resolve the run. Lines go
// to container stdout as structured JSON (run_id/agent_id tagged) and
// are kept by Airlock as the run's log record (a failed run's logs also
// feed the "Fix this error" builder). Use zap field constructors for
// structured context.
log := agent.Logger(ctx)
log.Info("imported rows", zap.Int("count", 42))
log.Warn("skipping row", zap.Int("row", i), zap.Error(err))
// Levels: Debug, Info, Warn, Error. import "go.uber.org/zap"

// Storage — see RegisterDirectory section. Trusted; no CheckFileAccess.
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
`agent.LLM(ctx, slug, ModelOpts)` and the `goai` package. Calls are proxied
through Airlock so token usage is tracked.

**Plain text:**

```go
import (
    "github.com/airlockrun/goai"
    "github.com/airlockrun/goai/message"
    "github.com/airlockrun/goai/stream"
)

func Summarize(ctx context.Context, in SummarizeIn) (SummarizeOut, error) {
    a := agentsdk.AgentFromContext(ctx)
    model := a.LLM(ctx, "summarize", agentsdk.ModelOpts{})

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
    model := a.LLM(ctx, "sentiment", agentsdk.ModelOpts{})

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

## Built-in VM bindings (DO NOT re-register)

The runtime system prompt fully documents JS bindings the LLM can use inside
`run_js`. Your only concern as an agent author is **don't shadow them with
`RegisterTool` names**. The auto-bound names per-agent depend on what you
register:

- `conn_{slug}` — for each `RegisterConnection` (request, requestJSON)
- `mcp_{slug}` — for each `RegisterMCP`; one method per discovered MCP tool,
  e.g. `mcp_github.search_repos({...})`
- `topic_{slug}` — for each `RegisterTopic` (subscribe, unsubscribe)

Framework-provided primitives (always present, the runtime prompt describes
each in detail):

- **File API** — `fileRead`, `fileReadBytes`, `fileWrite`, `fileStat`, `fileList`,
  `fileDelete`, `fileExists`, `fileShareURL`. Operate on S3-like storage with
  slashless paths (`uploads/x`, not `/uploads/x`); see the storage section
  above. `fileRead`/`fileReadBytes` cap at 16 MiB.
- **Large-file reads** — `fileReadRangeBytes(path, start, length)` (an exact
  byte window, no whole-file load; bytes only — text is addressed by line),
  `fileGrep(path, pattern, opts?)`, `fileHead(path, n?)`, `fileTail(path, n?)`,
  `fileLines(path, start, count)`. Large reads transparently cache to local disk for the run, so
  repeated scans of the same file don't re-fetch from S3.
- **File→file transforms** (authed runs only) — `fileEncode(src, codec, dst?)` /
  `fileDecode(src, codec, dst?)` (base64 | base64url | hex | gzip),
  `fileDecodeText(src, charset, dst?)` (charset → UTF-8). Stream the whole file
  with no size cap; omit `dst` for an auto scratch path.
- **File editors** (authed runs only) — `fileEditLines(src, edits, dst?)`
  (structured 1-based line edits: replace/delete/insert/append) and
  `fileSed(src, script, dst?)` (a sed subset: `s/re/repl/[gi]`, `d`, `c`/`i`/`a`,
  addresses `N`·`N,M`·`/re/`·`$`). Both stream so a file too big to load whole
  is still editable; omit `dst` for a scratch path or pass `dst === src` to edit
  in place.
- **AI / media helpers** (resolved via per-agent capability defaults — no slug
  needed) — `analyzeImage(path, question?)`, `transcribeAudio(path, opts?)`,
  `generateImage(prompt, opts?)`, `speak(text, opts?)`, `embed(texts)`.
- **User-facing output** — `printToUser(parts)`, `attachToContext(path)`.
- **HTTP / web** — `httpRequest(url, opts?)`, `webSearch(query, count?)`.
- **Logging** — `log(...)`, `console.log/warn/error`.
- **Database** (admin runs only) — `queryDB(sql, ...params)`,
  `execDB(sql, ...params)`.
- **Self-rebuild** (admin runs only) — `requestUpgrade(description)`.

Do not re-declare any of these as `RegisterTool`s.

**Public-caller surface is much narrower.** A run triggered by an
`AccessPublic` caller (unauthenticated bot, public route) only sees:

- `printToUser`, `log` / `console` — always present
- File API: each verb is bound only when at least one registered directory
  grants `AccessPublic` for the matching cap. `fileRead` / `fileReadBytes` /
  `fileReadRangeBytes` / `fileGrep` / `fileHead` / `fileTail` /
  `fileLines` / `fileStat` / `fileExists` / `fileShareURL` need a public **Read** dir;
  `fileWrite` / `fileDelete` need a public **Write** dir; `fileList` needs a
  public **List** dir. If you've registered no public dirs, the file API is
  invisible to public callers entirely.
- `conn_{slug}` / `mcp_{slug}` / `topic_{slug}` / registered tools — only the
  ones explicitly marked `Access: AccessPublic`

Bind-time-gated *out* for public callers: `httpRequest`, `webSearch`, AI/media
helpers (`analyzeImage`, `transcribeAudio`, `generateImage`, `speak`, `embed`),
file→file transforms (`fileEncode`, `fileDecode`, `fileDecodeText`), file
editors (`fileEditLines`, `fileSed`), `attachToContext`, `queryDB` / `execDB`,
`requestUpgrade`. These don't exist in
the JS runtime for public runs — they can't be coaxed into existence by prompt
injection, and they can't drive metered/external resources on a public
visitor's behalf.

Plan public flows around this: if a public visitor needs the agent to look up
or compute something, register a typed `Tool` with `Access: AccessPublic` that
performs the call internally with whatever auth/limits you choose. Don't expect
the LLM to reach for a primitive that isn't there.

**For public flows, narrow the surface to single-purpose tools rather than
registering a public directory and expecting the LLM to assemble several
primitives.** A single `Tool` you control gives you one place to validate the
input, sanitize the prompt, and decide what reaches the user — and the LLM only
sees the verb you want it to use. Concrete example: an "AI-generated image"
feature for public visitors. Don't do this:

```
RegisterDirectory("generated", AccessPublic for Read+Write+List)
// ... then hope the LLM stitches generateImage + fileWrite + fileShareURL
//     correctly on every call, with no rate limit, leaking the storage layout
```

Do this — one bounded tool, no public storage dir at all:

```go
type GenImageIn struct {
    Prompt string `json:"prompt" jsonschema:"description=Image description"`
}
type GenImageOut struct {
    URL string `json:"url" jsonschema:"description=Time-limited URL to the generated image"`
}

agent.RegisterTool(&agentsdk.Tool[GenImageIn, GenImageOut]{
    Name:        "generate_public_image",
    Description: "Generate an image from a prompt and return a 1-hour shareable link.",
    Access:      agentsdk.AccessPublic, // single verb the public LLM is allowed to call
    Execute: func(ctx context.Context, in GenImageIn) (GenImageOut, error) {
        // Validate / sanitize the prompt here before spending tokens.
        // Generate via agent.ImageModel(...) → write the bytes to an
        // admin-only directory with an LLMHint that hides it from the
        // model (still reachable from this trusted Go code via the file
        // API, which bypasses CheckFileAccess) → presign and return the URL.
        info, err := generateAndStore(ctx, in.Prompt)
        if err != nil {
            return GenImageOut{}, err
        }
        share, err := agent.ShareFileURL(ctx, info.Path, time.Hour)
        if err != nil {
            return GenImageOut{}, err
        }
        return GenImageOut{URL: share.URL}, nil
    },
})
```

The public visitor's LLM sees one tool: `generate_public_image({prompt})`. It
can't list, read, write, or delete arbitrary paths. It can't call `httpRequest`
or `webSearch` to do something else with the prompt. The image lives in an
internal directory the LLM can't enumerate; the only way out is the presigned
URL the tool returns. That's the model: **shrink the verbs, control the side
effects, surface the URL.**

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
download data that goes stale (GeoIP DBs, ClamAV signatures, cached ML
weights), the update happens at runtime in the container's writable layer and
is lost when the container gets reaped.

The fix is `agent.SyncDown` / `agent.SyncUp`: pair them in a cron handler to
persist runtime updates to the agent's S3-backed storage, then pull them back
at boot. The container's local copy is the working copy; S3 is the durable
record.

```go
// SyncDown(ctx, "state/bin/", "/var/agent/bin/")
//   ListDir(state/bin/, recursive) → for each remote file newer than
//   local: download, atomic-rename, chmod 0755, set local mtime to remote.
// SyncUp(ctx, "/var/agent/bin/", "state/bin/")
//   filepath.Walk(localDir) → for each local file newer than remote:
//   WriteFile, set local mtime to the resulting S3 LastModified.
```

Multi-replica is **last-writer-wins**: two replicas concurrently uploading the
same path converge to whichever finished last. That's correct for self-updates
(both replicas end up with the same new binary anyway). For shared mutable
state with concurrent writers, use the agent's Postgres schema instead — files
are for blobs, rows are for shared state.

**Worked example — keeping `bun` self-updating across container restarts.**
`setup.sh` seeds `/var/agent/bin/bun` into the image. The `bun-refresh` cron
fires `bun upgrade` weekly and syncs the new binary up to S3. On any container
restart (image rebuild, reap, crash), `SyncDown` pulls the latest bun back
down. A typed tool exposes the runtime to the LLM as `eval_js`.

**Health-check budget:** the platform expects `agent.Serve()` to start within
~15 seconds of container start. Anything slower than a few hundred ms —
`SyncDown`, third-party warm-up, model preload — must run in a goroutine
*after* you call `Serve`, not synchronously in `main()` before it. The
`eval_js` tool first call may have to retry if the goroutine hasn't finished,
but that's better than failing the boot health check.

```bash
# setup.sh
apt-get update && apt-get install -y --no-install-recommends curl unzip
mkdir -p /var/agent/bin
BUN_INSTALL=/var/agent/bin curl -fsSL https://bun.sh/install | bash
```

```go
// main.go
package main

import (
    "context"
    "os/exec"
    "strings"

    "github.com/airlockrun/agentsdk"
)

func main() {
    agent := agentsdk.New(agentsdk.Config{
        Description: "Runs JS snippets via bun; refreshes the binary weekly.",
    })

    // Admin-only bucket for binaries; the LLMHint steers the model away
    // even on admin runs while leaving the directory reachable from
    // trusted Go code (the file API bypasses CheckFileAccess), so
    // SyncDown/SyncUp work freely.
    agent.RegisterDirectory("state", agentsdk.DirectoryOpts{
        Read:  agentsdk.AccessAdmin,
        Write: agentsdk.AccessAdmin,
        List:  agentsdk.AccessAdmin,
        Description: "Self-updating binaries persisted across container restarts.",
        LLMHint:     "framework-managed binary cache; do not read, write, or list",
    })

    // Boot: pull the latest bun from S3 in the background so we don't
    // block Serve. First boot is a no-op when S3 is empty — the
    // image's seeded /var/agent/bin/bun stays in place until the
    // weekly cron lands the first upload.
    go func() {
        _ = agent.SyncDown(context.Background(), "state/bin/", "/var/agent/bin/")
    }()

    // Weekly: self-update bun, then push the new binary to S3.
    agent.RegisterCron(&agentsdk.Cron{
        Name:     "bun-refresh",
        Schedule: "0 3 * * 0",
        Handler: func(ctx context.Context, _ *agentsdk.EventWriter) error {
            if err := exec.CommandContext(ctx, "/var/agent/bin/bun", "upgrade").Run(); err != nil {
                return err
            }
            return agent.SyncUp(ctx, "/var/agent/bin/", "state/bin/")
        },
    })

    type EvalIn struct {
        Code string `json:"code" jsonschema:"description=JS expression to evaluate"`
    }
    type EvalOut struct {
        Result string `json:"result"`
    }
    agent.RegisterTool(&agentsdk.Tool[EvalIn, EvalOut]{
        Name:        "eval_js",
        Description: "Evaluate a JS expression with bun and return stdout.",
        Access:      agentsdk.AccessUser,
        Execute: func(ctx context.Context, in EvalIn) (EvalOut, error) {
            cmd := exec.CommandContext(ctx, "/var/agent/bin/bun", "-e", in.Code)
            out, err := cmd.CombinedOutput()
            if err != nil {
                return EvalOut{}, err
            }
            return EvalOut{Result: strings.TrimSpace(string(out))}, nil
        },
    })

    agent.Serve()
}
```

The same shape works for `uv self update`, `freshclam` (ClamAV signature DB
into `/var/agent/data/clamav/`), browser binaries for headless scraping, ML
model weights, anything that updates at runtime. Pick `state/bin/` for
executables, `state/data/` for everything else — it's just a convention so the
LLM-facing prompt's directory inventory reads cleanly.

---

# Database access

You have a full Postgres database available (well, a single schema, but you can
create as many tables in it as you like). Usually the database has pgvector
enabled, so you can create vector columns and use them together with
`agent.EmbeddingModel(ctx, slug, ModelOpts{})`.

If the agent needs its own database tables:

1. Migration files in `db/migrations/` (e.g. `00001_init.sql`)
2. Query files in `db/queries/` (e.g. `queries.sql`)
3. `sqlc generate` — produces Go code in `internal/db/`
4. Import `internal/db` in your code

Migrations run automatically at container startup via **goose**. Each `.sql`
file has Up and Down sections:

```sql
-- +goose Up
CREATE TABLE rooms (
    id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL
);

-- +goose Down
DROP TABLE rooms;
```

**Numbering:** zero-padded prefixes (`00001_init.sql`). Goose runs them in
numeric order.

**Go migrations** for operational work (rename S3 keys, backfill via HTTP, ...):
create a `.go` file in `db/migrations/`. Get the agent via
`agentsdk.AgentFromMigrationContext(ctx)`.

**Tx vs NoTx:**
- `goose.AddMigrationContext(up, down)` — wraps in a Postgres transaction.
  Default for short, DB-focused work.
- `goose.AddMigrationNoTxContext(up, down)` — no wrapping tx. Use when you
  (1) call slow external services (S3, HTTP) — don't hold a Postgres tx idle
  across them; or (2) need ops Postgres won't run in a tx
  (`CREATE INDEX CONCURRENTLY`, `VACUUM`, ...).

```go
// db/migrations/00002_rename_media.go
package migrations

import (
    "context"
    "database/sql"
    "path"

    "github.com/airlockrun/agentsdk"
    "github.com/pressly/goose/v3"
)

func init() {
    // NoTx: calls S3 in a loop; don't hold a Postgres tx open across slow external calls.
    goose.AddMigrationNoTxContext(Up00002, Down00002)
}

func Up00002(ctx context.Context, db *sql.DB) error {
    // Build-time validation runs migrations against a test DB without S3,
    // Airlock API, or connection credentials. Guard side effects so SQL still runs.
    if agentsdk.IsValidatingMigrations() {
        return nil
    }
    agent := agentsdk.AgentFromMigrationContext(ctx)
    files, err := agent.ListDir(ctx, "old/", agentsdk.ListOpts{Recursive: true})
    if err != nil {
        return err
    }
    for _, f := range files {
        src := string(f.Path)
        dst := "media/" + path.Base(src)
        if err := agent.CopyFile(ctx, src, dst); err != nil {
            return err
        }
        if err := agent.DeleteFile(ctx, src); err != nil {
            return err
        }
    }
    return nil
}

func Down00002(ctx context.Context, db *sql.DB) error { return nil }
```

`main.go` already blank-imports `db/migrations`, so `init()` fires
automatically.

**Guard external side effects.** Build-time validation runs the full migration
chain (up → down → up) against a test DB clone with no S3, Airlock API, or
connection credentials. Go migrations that touch external services must check
`agentsdk.IsValidatingMigrations()` and return early — but still run any
DB/schema work later migrations depend on.

**Validate after creating migrations** (Airlock builder; three env vars
`TEST_DB_URL` for goose, `TEST_DB_PSQL` for psql, `TEST_DB_SCHEMA` — skip if
`$TEST_DB_URL` is unset):

```bash
goose -dir db/migrations postgres "$TEST_DB_URL" up
goose -dir db/migrations postgres "$TEST_DB_URL" reset
goose -dir db/migrations postgres "$TEST_DB_URL" up

psql "$TEST_DB_PSQL" -c "SET search_path TO $TEST_DB_SCHEMA; SELECT table_name FROM information_schema.tables WHERE table_schema = '$TEST_DB_SCHEMA'"
```

The agent gets its own Postgres schema. Generated code uses pgx/v5.

**Using sqlc in Go:**

```go
db := agent.DB()
queries := internaldb.New(db) // import "agent/internal/db" as internaldb
users, err := queries.ListActiveUsers(ctx)
```

**Always use sqlc.** Never write raw `db.QueryRow`/`db.Exec` strings in Go.

---

# Verifying a build

After writing code, generate templ output and compile:

```bash
go tool templ generate
go build ./...
```

Run `sqlc generate` only if you created `.sql` files in `db/queries/`. The
Docker image build re-runs `templ generate`; you don't commit generated files.
