package agentsdk

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestBundledHTMX(t *testing.T) {
	data, err := bundledAssets.ReadFile("assets/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%x", sha256.Sum256(data)), "e484d9171a9db30a39c8f16e3d709d4137f3211c659f8e6125816635033d593f"; got != want {
		t.Fatalf("htmx asset SHA-256 = %s, want %s", got, want)
	}
	if got, want := Assets.HTMX, "/__air/assets/htmx-4.0.0.min.js"; got != want {
		t.Fatalf("Assets.HTMX = %q, want %q", got, want)
	}
}

func TestWebUIAsset(t *testing.T) {
	for _, tt := range []struct {
		name, dashboard string
		status          int
	}{
		{"https dashboard", "https://dashboard.example:8443/agents/app", http.StatusOK},
		{"URL payload stays outside origin", "https://dashboard.example:8443/agents/app?q=%22%3Cscript%3E#evil", http.StatusOK},
		{"missing sync", "", http.StatusServiceUnavailable},
		{"opaque URL", "javascript:alert(1)", http.StatusServiceUnavailable},
		{"credentials", "https://user:pass@dashboard.example", http.StatusServiceUnavailable},
		{"missing hostname", "https://:8443/", http.StatusServiceUnavailable},
		{"malformed URL", "https://%zz/", http.StatusServiceUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := New(Config{Description: "navigation test"})
			a.promptData = wire.PromptData{AgentDashboardURL: tt.dashboard}
			r := httptest.NewRequest(http.MethodGet, assetsPathPrefix+webUIAssetName, nil)
			r.SetPathValue("name", webUIAssetName)
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("Referer", "https://evil.example/path")
			w := httptest.NewRecorder()
			a.handleAsset(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache control = %q", got)
			}
			if tt.status != http.StatusOK {
				return
			}
			if got := w.Header().Get("Content-Type"); got != "application/javascript; charset=utf-8" {
				t.Fatalf("content type = %q", got)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("content type options = %q", got)
			}
			body := w.Body.String()
			if !strings.HasSuffix(body, "})(\"https://dashboard.example:8443\");\n") {
				t.Fatal("script is not bound to the synchronized dashboard origin")
			}
			if strings.Contains(body, "evil.example") {
				t.Fatal("request origin entered trusted script")
			}
			for _, want := range []string{"event.source !== window.parent", "event.origin !== trustedParentOrigin", "navigation.traverseTo(previous.key)", "entry.index === current.index - 1"} {
				if !strings.Contains(body, want) {
					t.Fatalf("script missing %q", want)
				}
			}
		})
	}
}

func TestUnknownWebUIAsset(t *testing.T) {
	a := New(Config{Description: "asset test"})
	for _, name := range []string{"workspace-navigation-v1.js", "web-ui-v2.js"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, assetsPathPrefix+name, nil)
			r.SetPathValue("name", name)
			w := httptest.NewRecorder()
			a.handleAsset(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
		})
	}
}

func TestWebUIStyleAsset(t *testing.T) {
	a := New(Config{Description: "presentation test"})
	r := httptest.NewRequest(http.MethodGet, assetsPathPrefix+webUIStyleName, nil)
	r.SetPathValue("name", webUIStyleName)
	w := httptest.NewRecorder()
	a.handleAsset(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/css; charset=utf-8",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(w.Body.String(), `html[data-airlock-embedded="true"] [data-airlock-standalone-only]`) ||
		!strings.Contains(w.Body.String(), "display: none !important") {
		t.Fatal("stylesheet missing embedded-only presentation rule")
	}
}
