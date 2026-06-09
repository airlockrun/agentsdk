package agentsdk

import (
	"embed"
	"io/fs"
	"net/http"
)

// assetsPathPrefix is the URL prefix every bundled asset is served from.
// Centralised so Assets.* (the public catalog) and handleAsset (the
// route) agree on one string.
const assetsPathPrefix = "/__air/assets/"

// htmxAssetName is the on-disk versioned filename handleAsset accepts.
// Kept next to the public Assets catalog so a bump touches one place.
const htmxAssetName = "htmx-" + HTMXVersion + ".min.js"

// Bundled framework JS — htmx minified, served by every agent at
// /__air/assets/htmx-{version}.min.js. The scaffold layout.templ
// references it so a fresh agent has working interactivity out of the
// box, same-origin (no CDN, no cross-origin script tags). Updating the
// version is an agentsdk-side bump: replace the file in assets/, bump
// the const below, and the SDK-bump mass-rebuild propagates to every
// agent.
//
// Styling is NOT bundled — each agent compiles its own Tailwind output
// at build time and serves it from /static/app.{hash}.css (the
// scaffold's `views/assets.go` + `main.go` register that route). Sharing
// one stylesheet across agents would lock every agent into the same
// theme; per-agent compilation lets each one brand itself.

//go:embed assets/htmx.min.js
var bundledAssets embed.FS

// HTMXVersion is the version of htmx the asset route serves.
const HTMXVersion = "2.0.10"

// Assets is the catalog of framework JS bundled with agentsdk and
// served same-origin under /__air/assets/. The path carries the
// embedded version segment ("htmx-2.0.10.min.js"), so bumping agentsdk
// yields a fresh URL that's never been browser-cached — the immutable
// Cache-Control on prior versions can stay in place. Use it in templ
// layouts:
//
//	<script src={ agentsdk.Assets.HTMX }></script>
//
// /__air/assets/* is framework-reserved. For your own static files
// (icons, images, page-specific CSS, fonts), embed them and serve via
// a RegisterRoute under a different prefix like /static/{name} (which
// is how the scaffold serves the compiled Tailwind stylesheet).
var Assets = struct {
	HTMX string // versioned path to the bundled htmx (e.g. /__air/assets/htmx-2.0.10.min.js)
}{
	HTMX: assetsPathPrefix + htmxAssetName,
}

// handleAsset serves a bundled asset by versioned filename. The path
// component is matched against a closed set (the currently-served
// versioned filenames), so any unknown or stale-version path returns
// 404. immutable Cache-Control is safe because the URL itself carries
// the version — a bump produces a new URL that the browser has never
// cached.
func (a *Agent) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var contentType, embedded string
	switch name {
	case htmxAssetName:
		contentType = "application/javascript; charset=utf-8"
		embedded = "assets/htmx.min.js"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(bundledAssets, embedded)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
