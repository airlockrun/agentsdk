# HTML UI assets and icons

> Companion to `/libs/agentsdk/REFERENCE.md` — read that first. Come here when your task involves templ pages, htmx, static files, or interface icons.

## Framework assets

agentsdk bundles htmx and exposes:

- `agentsdk.Assets.HTMX` — versioned URL such as
  `/__air/assets/htmx-4.0.0.min.js`. Use it in the layout head:
  `<script src={ agentsdk.Assets.HTMX }></script>`.
- `agentsdk.HTMXVersion` — the bundled version string.

The bundled asset is htmx 4 core, not the `htmax` extension bundle. htmx 4
uses explicit attribute inheritance (`hx-target:inherited`, for example),
colon-separated event names, and `event.detail.ctx` for request and response
state. It disables elements during a request with `hx-disable`; `hx-ignore`
disables htmx processing for a subtree. HTTP errors swap by default, while 204
and 304 are no-swap statuses. Use `hx-status` or `htmx.config.noSwap` when an
error response must not replace its target.

The generated layout adds 401 to `noSwap`. `WebUI` handles
`htmx:response:error` through `event.detail.ctx.response.status`, reloading the
page so Airlock can enter the authentication relay. The full-page navigation
does not carry `HX-Request`, so the listener cannot recursively reload itself.

The optional upstream `hx-csp` extension enforces nonce-based policy for htmx
inline-script and expression paths. It is not part of the bundled core asset;
an application that chooses it must serve and configure the extension together
with its own Content Security Policy.

`/__air/assets/*` is framework-reserved. Register agent-owned embedded files
with `RegisterStaticAsset`; the SDK serves them publicly from `/static/{name}`
with `Cache-Control: public, max-age=31536000, immutable` and
`X-Content-Type-Options: nosniff`. Names are one URL-safe path segment and
should contain a content hash whenever bytes can change. Unknown names return
404. Declarations are copied and frozen with all other registrations.

```go
agent.RegisterStaticAsset(&agentsdk.StaticAsset{
    Name:        views.AppCSSName, // e.g. app.01234567.css
    ContentType: "text/css; charset=utf-8",
    Data:        views.AppCSS,
})
```

## WebUI integration

Include `@agentsdk.WebUI(agentsdk.WebUIOptions{})` once in each full-page templ
layout head, not in partial responses. The scaffold includes it. It renders an
external SDK script and presentation stylesheet, with typed options encoded as HTML data attributes and no
inline JavaScript. It combines embedded workspace integration with htmx 401
recovery, without loading htmx or requiring it. Standalone pages install the 401
handler but do not establish a parent bridge. CSS light/dark behavior is
independent of this component.

Use `WebUIOptions{DisableNavigation: true}` to suppress Back capability without
disabling current-page reporting or authentication recovery. Existing apps may
adopt the component in their shared layout and remove their inline htmx 401
handler. Adoption is optional; apps without the component retain Home and Open
in browser controls, with Back disabled.

The SDK serves `/__air/assets/web-ui-v1.js` with `no-store`, bound
to the exact dashboard origin from the authenticated sync response's
`promptData.agentDashboardUrl`. Missing or invalid dashboard metadata returns
503. The child accepts messages only from `window.parent` at that origin; the
dashboard accepts messages only from its retained iframe at the exact app origin.
No referrer, query parameter, wildcard target origin or message-provided origin
establishes trust.

Its companion `/__air/assets/web-ui-v1.css` is also served with `no-store` and
contains only the embedded presentation rule, not app layout or theme styles.

### Embedded presentation and responsive layout

`data-airlock-*` is the reserved public SDK integration attribute namespace.
The marker requires `@agentsdk.WebUI(agentsdk.WebUIOptions{})` in the full-page
head and an app rebuilt against an SDK containing its script and stylesheet;
adding the attribute alone does not install integration.
Mark only compact app identity (name/description) with
`<header data-airlock-standalone-only>…</header>`; keep navigation, filters, and
actions outside it. The SDK stylesheet uses `display: none` for marked elements
only beneath `html[data-airlock-embedded="true"]`, leaving no reserved space or
DOM removal. The rule also covers dynamically inserted content.

The marked element owns the complete identity-only block: title, description,
logo, decorative background, borders, padding and margins. Do not mark only the
title inside a separate identity banner; the unmarked banner remains visible.
Avoid fixed-height wrappers or grid rows that reserve space for hidden identity.
Keep functional controls in a separate region that remains usable without the
identity block. Verify both standalone and trusted embedded views: the embedded
view should contain neither duplicate identity nor an empty decorative shell.

`data-airlock-embedded` on `<html>` is SDK-owned, read-only presentation state:
the bridge sets it to `"true"` only after a valid trusted-parent `probe`, in both
developer and production frames. Apps must not set it or use it for authorization.
Without that handshake, standalone and unrelated embedded pages remain visible;
identity can appear briefly before negotiation. No speculative pre-handshake
hiding, iframe ancestry detection, or theme attribute changes are involved.

The pane's width is dynamic. Use fluid widths, `min-width: 0`, wrapping grids and
CSS media/container queries against the live iframe viewport/container, not a
fixed iframe width or static mobile flag. Verify 375px and live narrow/wide pane
resizing without reload. Keep useful work first; standalone identity stays compact.

### Browser protocol contract

The browser-only discriminated union has `type: "airlock:web-ui:v1"` on every
message. Its runtime parser and TypeScript types live in Airlock's
`frontend/src/utils/appWebUI.ts`; the SDK implementation is `assets/web-ui.js`.
This is not a REST, WebSocket, or app-runtime wire protocol.

| Direction | `action` | Fields |
| --- | --- | --- |
| app → host | `ready` | none; discovery hint only |
| host → app | `probe` | `nonce` |
| app → host | `capabilities` | `nonce`, `features: string[]` |
| app → host | `state` | `nonce`, `url: string`, optional `canGoBack: boolean` |
| host → app | `back` | `nonce` |

The host probes on each iframe load with a fresh connection nonce (nonempty,
at most 128 characters). The app echoes it in capabilities and state; subsequent
commands require the same nonce. The host ignores state before negotiation,
unsupported versions, malformed payloads, stale nonces and invalid/foreign URLs.
Both ends check exact source and origin before parsing. `ready` only prompts a
probe and cannot establish capabilities. Authentication bootstrap/relay remains
host-owned, pre-app, and uses its separate source/origin-checked nonce protocol.

Features are independent: `location` enables validated current-page reporting;
`auth-recovery` describes the local htmx 401 listener; `navigation` enables Back
commands and boolean `canGoBack` state. Unknown feature names are ignored.
`navigation` is advertised only when enabled by options and the browser exposes
Navigation API entries and traversal. Missing features do not disable others.

State follows `currententrychange`, `popstate`, `hashchange` and `pageshow`.
Navigation API events cover full-page and History API/htmx navigation. Browsers
without that API report successful native `pushState`/`replaceState` URL changes
while keeping Back unavailable; no parallel history is retained. Back
traverses to the actual immediately previous same-origin entry, excluding
`/__air/` authentication/bootstrap pages. No guessed history counter is used.
Home navigates the retained iframe to `/`; Open in browser uses the validated
reported URL, or app home without location state.

## Route audit logging

`Route.Logging` controls audit detail and retention, not authentication or
capabilities. `LoggingDefault` is the zero value: GET/HEAD use `LoggingOff`, all
other methods use `LoggingNormal`. Unknown modes fail registration and sync.
Explicitly enable Normal for sensitive reads and exports involving corporate
secrets, PII or privileged downloads. Keep fragment reads Off. Do not mutate through GET.

```go
agent.RegisterTool(saveProfileTool, agentsdk.AccessAdmin)
agent.RegisterRoute(&agentsdk.Route{
    Method:      "POST",
    Path:        "/profile",
    Handler:     pages.SaveProfile,
    Access:      agentsdk.AccessUser,
    Description: "Save profile from the web form",
    Logging:     agentsdk.LoggingNormal,
})
```

The handler and registered tool invoke shared domain logic explicitly. Route and
tool permissions are independent. Airlock signs an opaque lazy dispatch proof;
reading `CallerFromContext` does not create a run or perform network I/O. An SDK
API requiring a run materializes exactly one record with the admitted actor.
All handlers share a 30-minute absolute deadline starting at host HTTP admission;
late materialization cannot renew it. Use registered jobs for durable work.

Normal retains one bounded route record for 30 days, including host-observed HTTP
status even without an SDK callback. HTTP status and execution status are separate;
a delivery failure is not business success. Audit contains the method and path
template, never automatic raw request/response bodies, parameter values, query
strings or headers. Explicit SDK logs and action telemetry are bounded; do not
put secrets or personal data in them. Off discards optional telemetry and creates
no run/origin/receipt if no API needs one; logging alone stays on container stdout
without materializing an Off route. Settled, dependency-free Off records
are eligible for cleanup after the absolute deadline plus one hour. Operational
jobs, subscriptions, transfers and run-scoped files pin their required records.

Telemetry limits apply independently of payload keys: at most 128 actions,
8 KiB per action and 64 KiB total action JSON; at most 256 log entries and
32 KiB serialized log JSON; 2 KiB error text and 8 KiB panic text. Oversized
action payloads are omitted while bounded action metadata remains. Text is
UTF-8/JSONB-safe. Required execution status and cost accounting survive detail
discarding, including when an omitted action failed.

`RegisterStaticAsset` owns the reserved `/static/` namespace. Its exact registered
`GET /static/{name}` declaration and bundled `/__air/assets/{name}` assets do not
create route runs. Application routes use their declared method policy regardless
of filename extension.

## Styling and automatic themes

Scaffolded apps use Tailwind v4 and DaisyUI v5. Configure a suitable built-in
light/dark theme pair in `styles/app.css`, with the light theme as `--default`
and the dark theme as `--prefersdark`. The scaffold uses:

```css
@plugin "../.airlock/toolchain/lib/tailwind/daisyui.mjs" {
  themes: light --default, dark --prefersdark;
}
```

Keep the root `<html lang="en">` free of `data-theme` and do not force a CSS
`color-scheme`. DaisyUI's `prefers-color-scheme` rules follow the embedding
Airlock page's `color-scheme` automatically; standalone pages follow the system
preference. No theme-sync JavaScript or app-specific toggle is needed.

Use semantic DaisyUI colors (`bg-base-100`, `text-base-content`,
`border-base-300`, `btn-primary`) rather than fixed palette colors so the UI
remains readable in both modes. Choose a pair that fits the app's tone, verify
both modes, and record the pair, font source and layout decisions in `NOTES.md`.
Read the version-matched DaisyUI config/colors skills for theme and token choices.
Plugin examples using bare `@plugin "daisyui"` need the repo-local toolchain path
above; do not install a separate npm copy. To retint a built-in theme, reuse its
name with `../.airlock/toolchain/lib/tailwind/daisyui-theme.mjs` and override only
the necessary tokens. A Tailwind `@theme` block does not retint DaisyUI components.

Build useful product screens rather than agent demonstrations: show domain data
and primary actions, not tool inventories, SDK/model names, internal IDs, chat
instructions or filler badges. Choose a domain-appropriate tone before markup;
keep branding compact and avoid per-element hardcoded colors. Verify light/dark
contrast and mobile layouts as well as embedded resizing.

## Lucide icons

The `github.com/airlockrun/agentsdk/lucide` package embeds the complete
version-pinned Lucide catalog in compressed form. `lucide.Icon` renders only the
selected SVG inline, so the browser does not download a full sprite or run an
icon JavaScript loader.

```templ
import "github.com/airlockrun/agentsdk/lucide"

templ SettingsLink() {
    <a class="btn btn-ghost" href="/settings">
        @lucide.Icon("settings", "size-4 shrink-0")
        <span>Settings</span>
    </a>
}
```

Names use Lucide's kebab-case identifiers. Unknown names and empty classes
panic so a typo cannot silently leave a blank slot. Every icon is emitted with
`fill="none"`, `stroke="currentColor"`, `aria-hidden="true"`, and
`focusable="false"`. Keep visible text on labeled controls. Put an `sr-only`
label on an icon-only control rather than labeling its decorative SVG.

The catalog is decompressed and indexed on the first `lucide.Icon` call. The
exact version-matched name list and the canonical htmx action-button pattern
are in `.airlock/toolchain/skills/lucide/SKILL.md`. The scaffold's
`views.ActionIcon` overlays a Lucide idle icon and DaisyUI spinner in one cell,
so the button has no blank idle slot and its label never shifts during a
request.

## Templ routes

### Package boundaries

`main.go` wires domain services, tool receivers and constructed HTTP handlers.
Keep domain logic in `<domain>/service.go`, typed bound tool methods in
`<domain>/tools.go`, HTTP controllers in `handlers/`, and templ components plus
view-model types in `views/`. A handler imports the domain and views, converts
domain values to view models, and renders. Domain code imports neither views nor
handlers; views import no domain packages. Both HTTP and tools invoke the same
service, including caller authorization and data scoping.

Use relative links such as `/settings` inside the UI. `agent.AgentURL()` (on your
`*agentsdk.Agent`) and
`agentsdk.AgentURLFromContext(ctx)` provide the synchronized public app origin for
emails, callbacks and chat links at runtime, not during definition or offline
inspection. Direct rendering tests can omit absolute links when unavailable.

Read the templ skill before markup: control flow has no early `return`, and static
JavaScript handlers are quoted literals (`onclick="my_modal.showModal()"`), not
Go string expressions. Prefer htmx for behavior; do not reach for unsafe script
helpers to work around an incorrectly typed attribute.

### Register a page

The scaffold wires the initial page to a constructed handler. Additional pages
follow the same route declaration pattern:

```go
import "agent/handlers"

pages := handlers.New(handlers.Deps{Spotify: spotifyService})
agent.RegisterRoute(&agentsdk.Route{
    Method:      "GET",
    Path:        "/",
    Handler:     pages.Home,
    Access:      agentsdk.AccessUser,
    Description: "Home page",
})
```

## Live updates without disrupting work

Declare a late-bound, **user-scoped** change topic in `newAgent()` and inject it
into the domain service shared by HTTP, tools and jobs:

```go
changes := agent.RegisterUIChangeTopic("tasks")
```

Names are static workflow labels, never user IDs, secrets or business content.

The service publishes in the **same app DB transaction** as its mutation. The
callback's `agentsdk.DBTX` value works with sqlc and `Publish`; passing a pool,
an independently opened transaction or another app's transaction returns an error.
Propagate every error:

```go
return database.Transaction(ctx, nil, func(tx agentsdk.DBTX) error {
    if err := db.New(tx).UpdateTask(ctx, params); err != nil { return err }
    return changes.Publish(ctx, tx, affectedUser.ID)
})
```

`affectedUser.ID` is the explicit human whose view changed, not an app-owner
fallback. Trusted domain code may invalidate an affected user's view from a job;
it remains responsible for mutation authorization and ownership. Put publishing
in the domain mutation, not just a tool wrapper. Raw SQL writes do not infer events.
Do not use this for arbitrary shared audiences or group ACLs.

Include `WebUI` once in the layout. Mark the **smallest read-only fragment**, even
a single HTML field or word, rather than replacing a page/card/form:

```html
<span data-airlock-topic="tasks" hx-get="/tasks/count"
      hx-trigger="airlock:change" hx-swap="innerHTML">12</span>
```

`data-airlock-topic` is a strict ASCII-whitespace-separated list of unique names
matching `[a-z][a-z0-9_-]{0,63}` (1–32 names; at most 32 distinct active names per
document). Invalid lists are ignored as a whole. SDK registration rejects invalid
or duplicate declarations. The stream rejects unknown topics and browser-supplied
audiences. Plain JavaScript apps can listen directly on each marked root for
`airlock:change`; `event.detail` is `{topic, revision}`. Events do not bubble and
carry no HTML, SQL or business data. htmx is optional.

WebUI maintains one authenticated, same-origin fetch/SSE connection in standalone
and embedded pages, independently of parent messaging. A single mutation observer
tracks partial insertion/removal and changed subscriptions. `pagehide` aborts the
stream and releases observers/timers; back/forward restoration reconnects. Bursts
coalesce, duplicate revisions are ignored, and failed streams reconnect with
bounded backoff. There are **no periodic fragment/backend reads** in the browser.
Each connection's initial snapshot refreshes active fragments once, closing the
SSR/connect window and catching up after disconnection. Do not add `load` to the
fragment trigger: the server-rendered content already exists. This handshake
refresh is deliberate, not an exactly-once mutation notification.

The SDK persists one opaque revision per user/topic in its app-local
`_airlock_ui_changes_v1` table. Both revision and PostgreSQL notification commit
with the mutation; rollback publishes nothing. LISTEN precedes snapshot reads.
Notifications only wake durable reads; listener reconnection rechecks revisions.
One shared LISTEN session serves each SDK process, with at most 64 concurrent
browser streams and a one-wakeup buffer per stream; slow readers coalesce rather
than accumulating event history. Stream writes have a five-second deadline.
Shutdown cancels streams and closes the listener. `AIRLOCK_DB_URL` must support a
session-scoped LISTEN connection, not a transaction-mode connection proxy.
There is no app-to-host durable transfer or cross-database atomicity claim: the
host proxies the app's durable snapshot stream using its existing exact-runtime
credential, signed route dispatch, live identity/app/deployment checks, deadline
and revocation watcher. Production and each preview have separate app schemas,
credentials and notification channels. Revisions survive app/host restarts. The
stream is an SDK-owned app-runtime SSE contract (`wire.UIChange`), not a dashboard
WebSocket payload. Repeated/coalesced refreshes are expected.

Authenticated fragment routes must apply the same live caller/ownership checks as
the page and tools. Keep filters and focused forms outside swap targets; use
explicit `hx-include="#filters"` on a list fragment so GET refreshes retain active
query/form values. Do not silently reset filters, pagination, focus or scroll.
An initiating POST can return 204 and let push refresh its read-only fragment, as
the scaffold's editable welcome word does. If it also returns the fragment, an
extra event-driven refresh is possible; return 204 for an unchanged event-driven
read if needed. Never add periodic polling to suppress duplicate refreshes.
