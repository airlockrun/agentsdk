package agentsdk

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
)

// assetsPathPrefix is the URL prefix every bundled asset is served from.
// Centralised so Assets.* (the public catalog) and handleAsset (the
// route) agree on one string.
const assetsPathPrefix = "/__air/assets/"

// htmxAssetName is the on-disk versioned filename handleAsset accepts.
// Kept next to the public Assets catalog so a bump touches one place.
const htmxAssetName = "htmx-" + HTMXVersion + ".min.js"

// Bundled framework assets — htmx minified, served by every agent at
// /__air/assets/htmx-{version}.min.js. The scaffold layout.templ
// references it so a fresh agent has working interactivity out of the
// box, same-origin (no CDN, no cross-origin script tags). Updating the
// version is an agentsdk-side bump: replace the file in assets/, bump
// the const below, and the SDK-bump mass-rebuild propagates to every
// agent.
//
// App styling is NOT bundled — each agent compiles its own Tailwind output
// at build time and registers it at /static/app.{hash}.css. Sharing
// one stylesheet across agents would lock every agent into the same
// theme; per-agent compilation lets each one brand itself.

//go:embed assets/htmx.min.js assets/web-ui.js assets/web-ui.css
var bundledAssets embed.FS

// HTMXVersion is the version of htmx the asset route serves.
const HTMXVersion = "4.0.0"

// Assets is the catalog of framework JS bundled with agentsdk and
// served same-origin under /__air/assets/. HTMX carries its embedded framework
// version in the URL and uses immutable caching. Use it in templ layouts:
//
//	<script src={ agentsdk.Assets.HTMX }></script>
//
// /__air/assets/* is framework-reserved. Use the lucide subpackage for the
// bundled icon catalog. Register other embedded images, page-specific CSS, and
// fonts with RegisterStaticAsset.
var Assets = struct {
	HTMX string // versioned path to the bundled htmx (e.g. /__air/assets/htmx-4.0.0.min.js)
}{
	HTMX: assetsPathPrefix + htmxAssetName,
}

// handleAsset serves a closed set of bundled filenames. Framework-versioned
// assets use immutable caching; WebUI's bridge and presentation rule use no-store.
func (a *Agent) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var contentType, embedded string
	switch name {
	case webUIAssetName:
		a.handleWebUI(w, r)
		return
	case webUIStyleName:
		contentType = "text/css; charset=utf-8"
		embedded = "assets/web-ui.css"
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
	if name == webUIStyleName {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// handleWebUI binds the script to the dashboard origin supplied
// by the authenticated sync response, independently of request headers or messages.
func (a *Agent) handleWebUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.syncMu.RLock()
	dashboardURL := a.promptData.AgentDashboardURL
	a.syncMu.RUnlock()
	u, err := url.Parse(dashboardURL)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		http.Error(w, "WebUI requires a synchronized dashboard URL", http.StatusServiceUnavailable)
		return
	}
	origin, err := json.Marshal(u.Scheme + "://" + u.Host)
	if err != nil {
		panic(err)
	}
	script, err := bundledAssets.ReadFile("assets/web-ui.js")
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte("(function(trustedParentOrigin) {\n"))
	_, _ = w.Write(script)
	_, _ = w.Write([]byte("\n})(" + string(origin) + ");\n"))
}
