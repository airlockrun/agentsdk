package filehttp

import (
	"mime"
	"net/http"
	"strings"
	"testing"
)

func TestSetHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, filename string
		download, sandbox           bool
		wantType, disposition       string
	}{
		{"html", "text/html; charset=utf-8", "diagram.html", false, true, "text/html", "inline"},
		{"svg", "IMAGE/SVG+XML", "diagram", false, true, "image/svg+xml", "inline"},
		{"xhtml", "application/xhtml+xml", "diagram.xhtml", false, true, "application/xhtml+xml", "inline"},
		{"xml", "text/xml", "document", false, true, "text/xml", "inline"},
		{"extension", "text/plain", "DIAGRAM.HTML", false, true, "text/plain", "inline"},
		{"download", "text/html", "diagram.html", true, true, "text/html", "attachment"},
		{"png", "image/png", "picture.png", false, false, "image/png", "inline"},
		{"jpeg", "image/jpeg", "picture.jpg", false, false, "image/jpeg", "inline"},
		{"missing mime", "", "file", false, true, "application/octet-stream", "attachment"},
		{"binary mime", "application/octet-stream", "file", false, true, "application/octet-stream", "attachment"},
		{"unknown mime", "application/x-unknown", "file", false, true, "application/x-unknown", "inline"},
		{"unknown text", "text/x-html", "file", false, true, "text/x-html", "inline"},
		{"bad mime", "text/html\r\nInjected: true", "file.html", false, true, "application/octet-stream", "attachment"},
		{"unusual name", "text/html", "diagram \"résumé\"\r\n.html", false, true, "text/html", "inline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			SetHeaders(h, tc.contentType, tc.filename, tc.download)
			mediaType, _, err := mime.ParseMediaType(h.Get("Content-Type"))
			if err != nil || mediaType != tc.wantType {
				t.Fatalf("content type=%q, error=%v", mediaType, err)
			}
			disposition, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
			if err != nil || disposition != tc.disposition || params["filename"] != tc.filename {
				t.Fatalf("disposition=%q params=%v error=%v", disposition, params, err)
			}
			if (h.Get("Content-Security-Policy") == SandboxPolicy) != tc.sandbox {
				t.Fatalf("CSP=%q", h.Get("Content-Security-Policy"))
			}
			if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
				t.Fatalf("headers=%v", h)
			}
			for _, values := range h {
				for _, v := range values {
					if strings.ContainsAny(v, "\r\n") {
						t.Fatal("unsafe raw header characters")
					}
				}
			}
		})
	}
}

func TestSandboxPolicyAndExistingRestrictions(t *testing.T) {
	directives := map[string]string{}
	for _, directive := range strings.Split(SandboxPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
		directives[name] = value
	}
	if directives["sandbox"] != "allow-scripts" {
		t.Fatal("sandbox must permit scripts without same-origin, navigation, forms or popup privileges")
	}
	for _, name := range []string{"default-src", "connect-src", "form-action", "base-uri"} {
		if directives[name] != "'none'" {
			t.Fatalf("%s=%s", name, directives[name])
		}
	}
	if directives["script-src"] != "'unsafe-inline'" || directives["style-src"] != "'unsafe-inline'" || directives["img-src"] != "data: blob:" {
		t.Fatalf("unexpected document capabilities: %v", directives)
	}
	h := make(http.Header)
	h.Set("Content-Security-Policy", "script-src 'none'")
	SetHeaders(h, "text/html", "example.html", false)
	if got := h.Values("Content-Security-Policy"); len(got) != 2 || got[0] != "script-src 'none'" || got[1] != SandboxPolicy {
		t.Fatalf("outer policy weakened: %v", got)
	}
}
