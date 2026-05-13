package agentsdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 204 No Content (or any zero-length 2xx body) from upstream is the
// failure mode that surfaced as "unexpected end of JSON input" in
// caller code that does its own json.Unmarshal on Request's bytes.
// RequestJSON skips the unmarshal on empty body and returns the
// zero T — matching what conn_<slug>.requestJSON does on the JS side.
func TestRequestJSON_EmptyBodyReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// no body
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ConnectionHandle{slug: "test", agent: a}

	type Playback struct {
		Track string `json:"track"`
	}
	got, err := RequestJSON[Playback](context.Background(), h, "GET", "/whatever", nil)
	if err != nil {
		t.Fatalf("RequestJSON returned %v, want nil on empty body", err)
	}
	if got.Track != "" {
		t.Errorf("got %+v, want zero Playback{}", got)
	}
}

// Non-empty body decodes into T as expected.
func TestRequestJSON_DecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"track":"hello"}`))
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ConnectionHandle{slug: "test", agent: a}

	type Playback struct {
		Track string `json:"track"`
	}
	got, err := RequestJSON[Playback](context.Background(), h, "GET", "/whatever", nil)
	if err != nil {
		t.Fatalf("RequestJSON returned %v", err)
	}
	if got.Track != "hello" {
		t.Errorf("got %+v, want Playback{Track:\"hello\"}", got)
	}
}
