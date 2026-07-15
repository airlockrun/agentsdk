package agentsdk

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticAssetHandler(t *testing.T) {
	a, _ := testAgent(t)
	data := []byte("body{}")
	a.RegisterStaticAsset(&StaticAsset{
		Name:        "app.01234567.css",
		ContentType: "text/css; charset=utf-8",
		Data:        data,
	})
	data[0] = 'X'
	handler := a.Handler()

	t.Run("registered asset", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/app.01234567.css", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
			t.Errorf("Content-Type = %q, want %q", got, "text/css; charset=utf-8")
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q, want immutable caching", got)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if !bytes.Equal(w.Body.Bytes(), []byte("body{}")) {
			t.Errorf("body = %q, want copied registration bytes", w.Body.Bytes())
		}
	})

	t.Run("unknown asset", func(t *testing.T) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/missing.css", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
}

func TestRegisterStaticAssetRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name  string
		asset *StaticAsset
		want  string
	}{
		{name: "nil declaration", want: "nil *StaticAsset"},
		{name: "empty name", asset: &StaticAsset{ContentType: "text/css", Data: []byte{}}, want: "Name must be one URL-safe path segment"},
		{name: "nested name", asset: &StaticAsset{Name: "css/app.css", ContentType: "text/css", Data: []byte{}}, want: "Name must be one URL-safe path segment"},
		{name: "empty content type", asset: &StaticAsset{Name: "app.css", Data: []byte{}}, want: "ContentType must be a valid media type"},
		{name: "invalid content type", asset: &StaticAsset{Name: "app.css", ContentType: "text/css\r\nX-Test: bad", Data: []byte{}}, want: "ContentType must be a valid media type"},
		{name: "nil data", asset: &StaticAsset{Name: "app.css", ContentType: "text/css"}, want: "Data is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testAgent(t)
			expectPanicContains(t, tt.want, func() {
				a.RegisterStaticAsset(tt.asset)
			})
		})
	}
}

func TestRegisterStaticAssetRejectsDuplicateAndFrozenRegistration(t *testing.T) {
	a, _ := testAgent(t)
	asset := &StaticAsset{Name: "app.css", ContentType: "text/css", Data: []byte("body{}")}
	a.RegisterStaticAsset(asset)
	expectPanicContains(t, "duplicate RegisterStaticAsset", func() {
		a.RegisterStaticAsset(asset)
	})

	a.Handler()
	expectPanicContains(t, "registrations are frozen", func() {
		a.RegisterStaticAsset(&StaticAsset{Name: "other.css", ContentType: "text/css", Data: []byte("html{}")})
	})
}
