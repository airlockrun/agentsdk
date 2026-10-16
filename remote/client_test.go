package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

const appID = "11111111-1111-1111-1111-111111111111"
const runtimeID = "22222222-2222-2222-2222-222222222222"
const ownerID = "33333333-3333-3333-3333-333333333333"
const revisionID = "44444444-4444-4444-4444-444444444444"
const runID = "55555555-5555-5555-5555-555555555555"

func configFor(server *httptest.Server) Config {
	return Config{URL: server.URL, AppID: appID, Target: "p-random", Revision: revisionID, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return "user-token", nil }}
}

func deploymentResponse(w http.ResponseWriter) {
	fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"personal_preview","name":"p-random","ownerUserId":%q,"sourceRevisionId":%q,"status":"active","running":true}}`, runtimeID, appID, runtimeID, ownerID, revisionID)
}

func TestExplicitConfigAndRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { deploymentResponse(w) }))
	defer server.Close()
	for _, change := range []func(*Config){
		func(c *Config) { c.Target = "preview" }, func(c *Config) { c.AppID = "" }, func(c *Config) { c.Token = nil },
		func(c *Config) { c.HTTPClient = nil }, func(c *Config) { c.URL += "/path" }, func(c *Config) { c.Target = "../production" },
	} {
		cfg := configFor(server)
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatal("accepted incomplete/unsafe selection")
		}
	}
	cfg := configFor(server)
	cfg.Revision = "different-revision"
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(t.Context()); err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatalf("revision error = %v", err)
	}
	cfg.Revision = ""
	client, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(t.Context(), "GET", "/", nil, nil); err == nil || !strings.Contains(err.Error(), "explicit source revision") {
		t.Fatalf("unpinned execution = %v", err)
	}
}

func TestRealOperatorWireAndNoRedirectFallback(t *testing.T) {
	base := "/api/v1/agents/" + appID + "/deployments/p-random"
	var invoked, requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Error("operator credential was not used")
		}
		switch r.URL.Path {
		case base:
			deploymentResponse(w)
		case base + "/tools/report/invoke":
			invoked++
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"period":"week"`) {
				t.Errorf("input = %s", raw)
			}
			fmt.Fprintf(w, `{"runId":%q,"resultJson":"{\"ok\":true}"}`, runID)
		case base + "/http/report":
			requests++
			if r.URL.RawQuery != "format=csv" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Location", "/production")
			w.WriteHeader(http.StatusFound)
		default:
			t.Errorf("unexpected target/fallback: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	input, _ := structpb.NewValue(map[string]any{"period": "week"})
	result, err := client.InvokeTool(t.Context(), "report", input)
	if err != nil || result.RunId != runID || invoked != 1 {
		t.Fatalf("tool = %v, %v", result, err)
	}
	response, err := client.Request(t.Context(), "GET", "/report?format=csv", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound || requests != 1 {
		t.Fatalf("response = %d", response.StatusCode)
	}
	for _, path := range []string{"https://production.example/", "//production.example/", "/ok#fragment", "relative"} {
		if _, err := client.Request(t.Context(), "GET", path, nil, nil); err == nil {
			t.Errorf("accepted unsafe path %s", path)
		}
	}
}

func TestAgentWaitCancellationDoesNotCancelWork(t *testing.T) {
	base := "/api/v1/agents/" + appID + "/deployments/p-random"
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		if r.URL.Path == base {
			deploymentResponse(w)
			return
		}
		if r.URL.Path != base+"/agents/summarize/runs/"+runID {
			t.Errorf("wrong path %s", r.URL)
		}
		fmt.Fprintf(w, `{"run":{"id":%q,"definition":"summarize","sessionId":%q,"status":"running","steps":1,"tokens":2,"createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:00Z"}}`, runID, runID)
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.WaitAgent(ctx, "summarize", runID, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v", err)
	}
	if deletes != 0 {
		t.Fatal("wait cancellation cancelled actual work")
	}
}

func TestUnauthorizedNeverSelectsProduction(t *testing.T) {
	var count int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; http.Error(w, "revoked", http.StatusForbidden) }))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(t.Context())
	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 403 || count != 1 {
		t.Fatalf("error/count = %v/%d", err, count)
	}
}

func TestDefaultProductionAndIndependentPreviewCreation(t *testing.T) {
	var gets, creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Airlock-Preview-Workspace") != "" || r.Header.Get("X-Airlock-Remote") != "" {
			t.Error("synthetic identity header")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/agents/" + appID + "/deployments/production":
			gets++
			fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production"}}`, appID, appID, appID)
		case "POST /api/v1/agents/" + appID + "/deployments/preview":
			creates++
			body, _ := io.ReadAll(r.Body)
			if string(body) != "{}" {
				t.Errorf("creation body contains identity: %s", body)
			}
			deploymentResponse(w)
		default:
			t.Errorf("unexpected target: %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	cfg := configFor(server)
	cfg.Target, cfg.Revision = "", ""
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := client.Get(t.Context()); err != nil || d.Name != "production" {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := client.CreatePreview(t.Context()); err != nil || d.Id != runtimeID || d.OwnerWorkspaceId != "" {
		t.Fatalf("creation: %v %v", d, err)
	}
	if d, err := client.Get(t.Context()); err != nil || d.Name != "production" || gets != 2 || creates != 1 {
		t.Fatalf("creation changed default: %v %v", d, err)
	}
}
