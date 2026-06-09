# agentsdk web UI guide (templ + htmx + Tailwind v4)

Read this whenever you create or modify a `.templ`, the Tailwind
source at `styles/app.css`, or anything else that affects how the
agent's HTML pages look or behave. The agentsdk reference (`llms.md`)
covers the APIs (`RegisterRoute`, `agentsdk.Assets.HTMX`, the scaffold
structure); this file covers Tailwind v4 conventions, the design
taste, and the htmx polish that separate a stock-demo page from
something opinionated.

## The build flow

```
styles/app.css           # source — you edit this
       │
       ▼  tailwindcss -i styles/app.css -o views/static/app.css --minify
views/static/app.css     # build output — never edit; gitignored
       │
       ▼  //go:embed in views/assets.go
views.AppCSS  +  views.AppCSSPath  ("/static/app.{hash}.css")
       │
       ▼  layout.templ
<link rel="stylesheet" href={ views.AppCSSPath }/>
```

The Docker build runs `templ generate && tailwindcss ... && go build`
in that order. The toolserver runs the same chain during codegen
iteration. Whenever you change a `.templ` *or* `styles/app.css`, run
both generators before `go build`:

```bash
go tool templ generate
tailwindcss -i styles/app.css -o views/static/app.css --minify
go build -o /tmp/agent .
```

The hash in `views.AppCSSPath` is derived from the compiled bytes, so
every rebuild yields a fresh URL — the immutable Cache-Control on
prior URLs stays correct.

## Tailwind v4 essentials

The whole config lives in `styles/app.css`:

```css
@import "tailwindcss";

@theme {
  --color-brand: oklch(0.7 0.18 145);
  --color-brand-dark: oklch(0.58 0.18 145);
  --font-display: "YourDisplay", system-ui, sans-serif;
  --font-sans: "YourBody", system-ui, sans-serif;
  --radius-card: 0.75rem;
}
```

Every `@theme` token automatically produces matching utility classes
(`text-brand`, `bg-brand-dark`, `font-display`, `rounded-card`, …).
Brand by adding tokens here; don't write per-element overrides in
templ files.

Content scanning is automatic — Tailwind v4 walks the project tree
for class-name-like strings, so anything you write in `.templ`
files gets picked up.

Hand-rolled components belong in this same file under `@layer
components`, NOT in `<style>` blocks inside `.templ`:

```css
@layer components {
  .card-action {
    @apply rounded-card border border-neutral-200 bg-white shadow-sm
           transition hover:shadow-md;
  }
}
```

That keeps the compiler aware of them and avoids style fragmentation.

## Don't ship default Tailwind

A page that's `flex flex-col gap-4` of three cards and a centered
heading is the same stock-demo trap pico used to fall into — just
written in different vocabulary. Decide what the page should *feel*
like before you write markup, grounded in the agent's domain: a
Spotify dashboard earns album art, a coloured progress bar, a dark
surface that nods to the brand without copying it; a weather agent
earns sky gradients keyed to the forecast; an admin console earns
restrained density and clear hierarchy. Then build to that decision.

### Pick a palette and put it in `@theme`

Don't sprinkle `bg-[#1db954]` throughout templates. Add the colour
as a token in `@theme`, use the generated utility class everywhere:

```css
@theme {
  --color-brand: oklch(0.72 0.18 145);
  --color-brand-hover: oklch(0.78 0.16 145);
  --color-surface: oklch(0.16 0.01 250);
  --color-surface-elevated: oklch(0.20 0.01 250);
  --color-ink: oklch(0.95 0.005 250);
  --color-ink-muted: oklch(0.70 0.01 250);
}
```

Then `bg-surface text-ink`, `bg-brand hover:bg-brand-hover`,
`border-surface-elevated`. A token swap re-themes the whole agent.
Prefer `oklch()` over hex — it gives perceptually uniform lightness
across hues, which is what makes a palette feel "designed."

For dark mode, add `dark:` variants — Tailwind v4 reads the system
preference by default. If your design is dark-first, set the
surface tokens to dark values directly; you don't need `dark:`.

### Replace the font stack

The default `font-sans` is `system-ui`, which ships generic.

1. Drop a `.woff2` file under `views/static/fonts/` and add an
   `@font-face` rule in `styles/app.css`:

   ```css
   @font-face {
     font-family: "YourBody";
     src: url("/static/fonts/yourbody.woff2") format("woff2");
     font-display: swap;
   }
   ```

2. Add a Tailwind token so the utility classes exist:

   ```css
   @theme {
     --font-sans: "YourBody", system-ui, sans-serif;
     --font-display: "YourDisplay", system-ui, sans-serif;
   }
   ```

3. The static asset route is already registered for
   `/static/{name}` — extend the handler in `main.go` to serve font
   bytes alongside `app.css` (one switch, same pattern), or scope
   the font under a different sub-handler.

Don't link Google Fonts. Same-origin self-hosting removes the
privacy leak and puts the page and the font in one reliability
domain.

### Use semantic HTML *and* opinionated layout

Tailwind doesn't style elements for you, so semantic HTML alone
doesn't carry you — you have to actually *compose* the page. The
shapes that earn their keep:

- `grid grid-cols-[200px_1fr] gap-6` — sidebar/main, or media/info.
- `flex items-center gap-3` — inline icon + label, button row.
- `space-y-4` on a card column — consistent vertical rhythm.
- `<progress class="h-1.5 w-full appearance-none ...">` — slim,
  branded; don't render text like `"0:48 / 3:00"`.

Reach for `<article>`, `<section>`, `<header>`, `<footer>`,
`<progress>`, `<details>`/`<summary>`, `<dialog>` as the
*structure*; layer Tailwind utilities for *layout and finish*.

### Button rank by intent, not by colour

Two buttons next to each other should look different only when they
mean different things. Common shape:

```html
<button class="rounded-md bg-brand px-4 py-2 font-medium text-white
               hover:bg-brand-hover">Primary action</button>
<button class="rounded-md border border-neutral-300 px-4 py-2 text-neutral-700
               hover:bg-neutral-100">Secondary</button>
```

A third "tertiary" usually means it's actually a link — use one. If
two buttons would express the same state (Play and Pause), render
only the contextually correct one based on state, not both side by
side.

### Show the domain, don't describe it

A music dashboard should render album art (most music APIs return
image URLs — `<img>` them at a generous size in a `grid` next to the
track info). A weather agent should render an SVG of the current
condition. Emoji-as-title-decoration ("🎧 Spotify Dashboard") is the
cheapest possible visual move; treat it as a placeholder you
replaced.

## Don't double-wrap swap targets

A common htmx + templ failure mode is to wrap the swap container in
a card landmark, then have the swapped partial *also* render a card.
You get two stacked headers, two borders, two paddings:

```html
<!-- WRONG: the outer card and the inner card both render -->
<section>
  <article class="card">
    <header>Now Playing</header>
    <div id="status-panel" hx-get="/partials/status">
      <article class="card">…</article>   <!-- StatusPanel returns this -->
    </div>
  </article>
</section>
```

Fix: let the swapped fragment *be* the card. The page renders the
empty target; the partial owns its own landmark:

```html
<section>
  <div id="status-panel"
       hx-get="/partials/status"
       hx-trigger="load, every 15s">
    <article class="card animate-pulse">Loading…</article>
  </div>
</section>
```

```go
templ StatusPanel(status spotify.PlaybackStatus) {
  <div id="status-panel"
       hx-get="/partials/status"
       hx-trigger="every 15s">
    <article class="card">
      <header>{ headerText(status) }</header>
      …
    </article>
  </div>
}
```

Same id on the wrapper so `hx-swap="outerHTML"` (the default) keeps
the polling attributes alive across swaps.

## htmx polish that costs almost nothing

- `hx-indicator` on every request to surface loading state (a
  pulsing dot, a subtle bar — not a spinner gif). Use Tailwind:
  `<span class="htmx-indicator inline-block h-2 w-2 animate-pulse
  rounded-full bg-brand"></span>`.
- `hx-swap="outerHTML transition:true"` to opt into the View
  Transitions API for state changes; the browser cross-fades for
  free.
- `hx-trigger="every 5s"` for any live data (now-playing, queue) so
  the page is *alive* without the user reloading. Pair with
  `hx-trigger="load, every 5s, refresh from:body"` plus
  `htmx.trigger('body', 'refresh')` from action handlers to bump the
  refresh immediately after a state-changing call.
- Staggered entrance: `class="motion-safe:animate-in
  motion-safe:fade-in motion-safe:slide-in-from-bottom-2"` on each
  `<article>`, plus an inline `style="animation-delay: 80ms"` keyed
  off the index.

## The check at the end

Describe your page in one sentence. If that sentence is *"a centered
title and some buttons"*, you shipped the default. If it's *"the
current track's album art fills the left third, a slim accent-
coloured progress bar runs under the title, playback controls sit
in a single row with one primary action"* — you designed something.

## Record the styling direction in NOTES.md

The chosen tone, the palette (with the actual `@theme` token values
you set), the font families and where they're loaded from, and any
layout conventions you committed to. A future upgrade pass picks up
`NOTES.md` before touching templates; without it, the next rebuild
silently drifts back to default-Tailwind taste.
