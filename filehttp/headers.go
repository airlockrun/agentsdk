// Package filehttp supplies browser response policy for untrusted stored files.
// Apply it when streaming file bytes, not when serving an application's UI.
package filehttp

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

// SandboxPolicy permits self-contained interactive documents without the
// application's origin privileges. It permits inline scripts/styles and embedded
// images, but not external scripts/resources, fetches, forms, workers or frames.
// Omitting allow-same-origin is essential: adding it restores app-origin access.
const SandboxPolicy = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; connect-src 'none'; form-action 'none'; base-uri 'none'"

// SetHeaders prepares a stored-file response before any bytes or status are
// written. download selects attachment disposition; otherwise active documents
// render inline under SandboxPolicy. Missing/invalid MIME types download as
// application/octet-stream. Stored metadata and bytes are never modified.
// Additional CSP policies are retained, so this cannot weaken an outer policy.
func SetHeaders(header http.Header, contentType, filename string, download bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.Contains(mediaType, "/") {
		mediaType, params = "application/octet-stream", nil
		download = true
	}
	if mediaType == "application/octet-stream" {
		download = true
	}
	header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	if needsSandbox(mediaType, filename) {
		header.Add("Content-Security-Policy", SandboxPolicy)
	}
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	if filename == "" {
		header.Set("Content-Disposition", disposition)
	} else {
		name := path.Base(strings.ReplaceAll(filename, "\\", "/"))
		header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	}
}

func needsSandbox(mediaType, filename string) bool {
	switch strings.ToLower(path.Ext(filename)) {
	case ".html", ".htm", ".xhtml", ".xht", ".svg", ".svgz", ".xml", ".xsl", ".xslt":
		return true
	}
	// Only explicitly passive formats omit the document sandbox. Unknown MIME
	// types can trigger browser sniffing; nosniff alone is not a navigation policy.
	switch mediaType {
	case "text/plain", "application/json", "text/css", "text/javascript", "application/javascript",
		"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon",
		"audio/mpeg", "audio/wav", "audio/ogg", "audio/webm", "video/mp4", "video/mpeg", "video/webm", "video/ogg":
		return false
	}
	return true
}
