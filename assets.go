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

// Versioned filenames matched by handleAsset. Kept here next to the
// public Assets catalog so a bump touches one place.
const (
	htmxAssetName = "htmx-" + HTMXVersion + ".min.js"
	picoAssetName = "pico-" + PicoVersion + ".min.css"
)

// Bundled frontend assets — htmx and pico.css minified, served by every
// agent at /__air/assets/{filename}. The scaffold layout.templ references
// them so a fresh agent has working interactivity and styling out of the
// box, same-origin (no CDN, no cross-origin script tags). Updating the
// version is an agentsdk-side bump: replace the file in assets/, bump the
// const below, and the SDK-bump mass-rebuild propagates to every agent.

//go:embed assets/htmx.min.js assets/pico.min.css
var bundledAssets embed.FS

// HTMXVersion is the version of htmx the asset route serves.
const HTMXVersion = "2.0.10"

// PicoVersion is the version of pico.css the asset route serves.
const PicoVersion = "2.1.1"

// Assets is the catalog of static assets bundled with agentsdk and
// served same-origin under /__air/assets/. Paths carry the embedded
// version segment ("htmx-2.0.10.min.js"), so bumping agentsdk yields
// a fresh URL that's never been browser-cached — the immutable
// Cache-Control on prior versions can stay in place. Use the fields in
// templ layouts:
//
//	<script src={ agentsdk.Assets.HTMX }></script>
//	<link rel="stylesheet" href={ agentsdk.Assets.Pico }/>
//
// /__air/assets/* is framework-reserved. For your own static files
// (icons, images, page-specific CSS, fonts), embed them and serve via
// a RegisterRoute under a different prefix like /static/*.
var Assets = struct {
	HTMX string // versioned path to the bundled htmx (e.g. /__air/assets/htmx-2.0.10.min.js)
	Pico string // versioned path to the bundled pico.css (e.g. /__air/assets/pico-2.1.1.min.css)
}{
	HTMX: assetsPathPrefix + htmxAssetName,
	Pico: assetsPathPrefix + picoAssetName,
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
	case picoAssetName:
		contentType = "text/css; charset=utf-8"
		embedded = "assets/pico.min.css"
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
