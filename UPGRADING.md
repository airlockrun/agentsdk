# Agent SDK upgrade instructions

This file contains source migrations that Airlock can apply while rebuilding an
application. Each entry starts at the first SDK release that requires the
change. Compatible releases without source changes do not need an entry.

## v0.8.1-alpha.8

Declare every app-storage prefix used by native Go code with
`RegisterDirectory`. Use `AccessInternal` for private native-only paths, and
preserve the appropriate human access policies and `Scope` for paths exposed to
users or untrusted callers. Native methods such as `WriteFile`, `OpenFile`,
`ListDir`, `CopyFile`, `ShareFileURL`, `SyncUp`, and `SyncDown` reject paths that
do not belong to a registered directory.

Do not register a broad synthetic prefix to cover unrelated paths. Declare the
actual storage roots so the catalog, Files visibility, and longest-prefix nested
directory policy remain consistent.

## v0.8.1-alpha.7

Update applications that use htmx from htmx 2 to the bundled htmx 4 core asset.
Applications without htmx usage require no source changes, but still verify the
application with the normal generated-code, test, and build checks.

Read `/libs/agentsdk/reference/html-ui.md`, then read the version-matched
`.airlock/toolchain/skills/htmx/SKILL.md` and
`.airlock/toolchain/skills/htmx/reference/htmx-upgrade-from-htmx2.md` before
editing. Inspect all `hx-*` and `data-hx-*` attributes, htmx event listeners,
JavaScript API calls, and server-side `HX-*` header handling. Use the bundled
`agentsdk.Assets.HTMX` URL rather than a htmx 2 asset or an extension bundle.

Apply the attribute renames in this order: first replace htmx 2 `hx-disable`
with `hx-ignore`, then replace `hx-disabled-elt` with htmx 4 `hx-disable`.
Add explicit `:inherited` modifiers only where descendants rely on a parent's
attribute. Convert event names to colon-separated htmx 4 names and update event
handlers to use request and response state under `event.detail.ctx`. Review
error responses because htmx 4 swaps HTTP error bodies by default; use
`hx-status`, `hx-swap="none"`, or `htmx.config.noSwap` where an error must not
replace its target.

Preserve the generated authentication behavior: 401 remains a no-swap status,
and the `htmx:response:error` handler reads
`event.detail.ctx.response.status` and reloads the page so Airlock can enter the
authentication relay. Preserve application routes, styling, data contracts,
and unrelated behavior.
