---
name: lucide
description: Lucide SVG icons bundled with Agents SDK for templ UIs. TRIGGER when adding or changing buttons, links, navigation, menus, status visuals, empty states, or loading indicators.
metadata:
  version: 1.34.0
  source: https://lucide.dev/icons/
---

# Lucide icons

Agents SDK carries the complete Lucide catalog and renders only selected icons
inline. There is no CDN, JavaScript icon loader, npm dependency, sprite request,
or copied SVG path.

## Render an icon

Import `github.com/airlockrun/agentsdk/lucide` in the templ file and provide a
valid kebab-case icon name plus classes that set its size:

```templ
import "github.com/airlockrun/agentsdk/lucide"

templ SettingsLink() {
	<a class="btn btn-ghost" href="/settings">
		@lucide.Icon("settings", "size-4 shrink-0")
		<span>Settings</span>
	</a>
}
```

`lucide.Icon` emits an inline `currentColor` SVG with `aria-hidden="true"`.
Unknown names and empty classes panic deliberately instead of leaving invisible
space. Use an exact name from [reference/icons.md](reference/icons.md).

## Action buttons

Every labeled action button has a domain-appropriate idle icon. For an htmx
action, use the scaffold's `ActionIcon` so the idle icon and DaisyUI spinner
occupy one fixed-size grid cell:

```templ
<button
	type="button"
	class="btn btn-primary"
	hx-post="/publish"
	hx-target="#publish-result"
	hx-disabled-elt="this"
>
	@ActionIcon("send")
	<span>Publish</span>
</button>
```

Do not add a hidden spinner as a separate flex child: it reserves blank space
while idle. Do not set `hx-indicator` to a remote element for this pattern;
htmx's `htmx-request` class must remain on the requesting button.

Add this component when `views/icons.templ` is absent:

```templ
package views

import "github.com/airlockrun/agentsdk/lucide"

templ ActionIcon(name string) {
	<span class="air-action-icon">
		@lucide.Icon(name, "air-action-icon-idle")
		<span class="loading loading-spinner air-action-icon-busy" role="status" aria-live="polite">
			<span class="sr-only">Working</span>
		</span>
	</span>
}
```

Add the matching rules to `styles/app.css`:

```css
.air-action-icon {
  display: inline-grid;
  width: 1.2em;
  height: 1.2em;
  flex: none;
}
.air-action-icon > * {
  grid-area: 1 / 1;
  width: 100%;
  height: 100%;
}
.air-action-icon-busy {
  visibility: hidden;
  animation-play-state: paused;
}
.htmx-request > .air-action-icon > .air-action-icon-idle { visibility: hidden; }
.htmx-request > .air-action-icon > .air-action-icon-busy {
  visibility: visible;
  animation-play-state: running;
}
@media (prefers-reduced-motion: reduce) {
  .air-action-icon-busy { animation: none; }
}
```

## Accessibility

- Keep visible text on labeled buttons; the icon is decorative.
- Put an `sr-only` label inside an icon-only button. Do not label its SVG.
- Do not communicate status through an icon or color alone.
- Keep interactive targets at least 44 by 44 CSS pixels where practical.

## Selection

Choose icons by meaning, not decoration. Common actions include `circle-plus`,
`download`, `external-link`, `pencil`, `refresh-cw`, `search`, `send`, `settings`,
`share-2`, `trash-2`, and `upload`. Use one icon consistently for one action
within an app. Lucide is for interface symbols, not third-party brand logos.
