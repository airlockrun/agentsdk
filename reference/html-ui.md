# HTML UI assets and icons

> Companion to `/libs/agentsdk/REFERENCE.md` — read that first. Come here when your task involves templ pages, htmx, static files, or interface icons.

## Framework assets

agentsdk bundles htmx and exposes:

- `agentsdk.Assets.HTMX` — versioned URL such as
  `/__air/assets/htmx-2.0.10.min.js`. Use it in the layout head:
  `<script src={ agentsdk.Assets.HTMX }></script>`.
- `agentsdk.HTMXVersion` — the bundled version string.

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
