package agenttest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
)

// TestEnv_RouteRoundTrip exercises the full helper surface: NewEnv wires the
// env, agentsdk.New builds the agent, and a registered route is reachable
// through Handler() with its {name} path param correctly extracted.
func TestEnv_RouteRoundTrip(t *testing.T) {
	agenttest.NewEnv(t)

	a := agentsdk.New(agentsdk.Config{Description: "test agent"})
	a.RegisterRoute(&agentsdk.Route{
		Method: "GET",
		Path:   "/static/{name}",
		Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
			if r.PathValue("name") == "app.css" {
				w.Header().Set("Content-Type", "text/css")
				_, _ = w.Write([]byte("body{}"))
				return
			}
			http.NotFound(w, r)
		},
		Access:      agentsdk.AccessPublic,
		Description: "static",
	})

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/css" {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "body{}" {
		t.Errorf("body = %q, want %q", body, "body{}")
	}

	miss, err := http.Get(srv.URL + "/static/bogus.css")
	if err != nil {
		t.Fatal(err)
	}
	miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Errorf("bogus status = %d, want 404", miss.StatusCode)
	}
}
