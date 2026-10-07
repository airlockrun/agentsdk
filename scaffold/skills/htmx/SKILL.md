---
name: htmx
description: htmx — HTML-over-the-wire interactivity via hx-* attributes, the interactivity layer for this agent's templ web UI. TRIGGER when adding or debugging any hx-* attribute (hx-get/post/swap/target/trigger), polling, partial page updates, or when a swap duplicates, nests, or fails to update the DOM.
metadata:
  version: v4.0.0
  source: https://github.com/bigskysoftware/htmx/tree/4195bc0dc26b612ea5bea46f5914c6386eadeba3/dist/skills
---

# htmx

htmx lets HTML drive AJAX, swaps, and polling through `hx-*` attributes. The
server returns HTML fragments; htmx swaps them into the DOM. It is served
same-origin by the framework (`agentsdk.Assets.HTMX`) — no CDN, no npm.

## When to run this skill

- Adding interactivity to a templ page (`hx-get`, `hx-post`, `hx-trigger`, …).
- A swap behaves wrong: content duplicates, nests inside itself, replaces the
  wrong element, or a poll never stops / resets the page.

## The rules that bite (read before writing hx-*)

- **Decide who owns the swap target, once.** Either the parent element swaps its
  own contents (`hx-target="this" hx-swap="innerHTML"`) **or** the response
  fragment is the new element — never both. Double-wrapping (a fragment that
  re-emits its own container into a container) is the usual cause of duplicated or
  nested cards. See [the upstream guidance](./reference/htmx-guidance.md)
  (swapping and targets).
- **For polling, return `204 No Content` when nothing changed.** htmx treats 204 as
  "do nothing", so an idle page stays put. Make each poll carry what it last saw
  (a query param/header) and return `200` + fresh HTML only on a real change.
  Both 204 and 304 skip swaps; use 204 for application-level no-change responses
  and reserve 304 for standard HTTP conditional-request semantics.
- **Inheritance is explicit.** Put `:inherited` on an attribute only when
  descendant request elements rely on the parent value, such as
  `hx-target:inherited="#results"`.
- **HTTP errors swap by default.** Use `hx-status`, `hx-swap="none"`, or
  `htmx.config.noSwap` when an error body must not replace the target. Events
  use colon-separated names and request state lives under `event.detail.ctx`.
- **`hx-disable` disables controls during requests.** `hx-ignore` prevents htmx
  processing for a subtree.
- **`hx-swap` controls placement** (`innerHTML` default, `outerHTML`, `beforeend`,
  …) and out-of-band updates use `hx-swap-oob`. Pick the mode deliberately; the
  default `innerHTML` replaces children, `outerHTML` replaces the element itself.
- **Triggers are explicit.** `hx-trigger` (`click`, `load`, `every 2s`,
  `revealed`, modifiers like `delay:`, `from:`) decides when a request fires; the
  default depends on the element. Set it rather than relying on defaults.

## Mandatory reference

| Task | Guide |
|------|-------|
| Core attributes, events, swaps, inheritance, and patterns | [./reference/htmx-guidance.md](./reference/htmx-guidance.md) |
| Migrating htmx 2 source to htmx 4 | [./reference/htmx-upgrade-from-htmx2.md](./reference/htmx-upgrade-from-htmx2.md) |
| Diagnosing requests and swaps | [./reference/htmx-debugging.md](./reference/htmx-debugging.md) |
| Authoring htmx 4 extensions | [./reference/htmx-extension-authoring.md](./reference/htmx-extension-authoring.md) |

These are the upstream htmx 4 skill documents pinned to the immutable revision
recorded in the bundle manifest. Read the relevant guide before wiring an
interaction, and pair it with the `templ` skill (htmx attributes live in `.templ`
markup, and partial responses are templ fragments).
