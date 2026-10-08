package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

func TestHostedResourceRequestCreatesCredentialFreeSetup(t *testing.T) {
	const (
		sessionID  = "11111111-1111-1111-1111-111111111111"
		agentID    = "22222222-2222-2222-2222-222222222222"
		resourceID = "33333333-3333-3333-3333-333333333333"
		requestID  = "44444444-4444-4444-4444-444444444444"
		runID      = "55555555-5555-5555-5555-555555555555"
	)
	var createCalls, requestCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer developer-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/resources/connections/setup":
			createCalls++
			var request airlockv1.CreateConnectionResourceRequest
			body := json.NewDecoder(r.Body)
			var raw map[string]any
			if err := body.Decode(&raw); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(raw)
			if err := protoUnmarshal.Unmarshal(encoded, &request); err != nil {
				t.Fatal(err)
			}
			if request.Token != "" || request.AuthMode != "token" || request.AuthInjectionType != "api_key_header" || request.AuthInjectionName != "X-API-Key" {
				t.Errorf("request = %+v", &request)
			}
			fmt.Fprintf(w, `{"id":%q,"slug":"res-test"}`, resourceID)
		case "/api/v1/developer-sessions/" + sessionID + "/resources":
			requestCalls++
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["developerRunId"] != runID {
				t.Errorf("developerRunId = %v", request["developerRunId"])
			}
			fmt.Fprintf(w, `{"request":{"id":%q,"sessionId":%q,"type":"connection","requestedSlug":"weather","resourceId":%q,"status":"setup_required","setupUrl":%q}}`, requestID, sessionID, resourceID, srv.URL+"/settings/resources?resource="+resourceID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", sessionID)
	t.Setenv("AIRLOCK_API_URL", srv.URL)
	t.Setenv("AIRLOCK_AGENT_ID", agentID)
	t.Setenv("AIRLOCK_TOKEN", "developer-token")
	t.Setenv("AIRLOCK_DEVELOPER_RUN_ID", runID)
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	output, err := captureCommandStdoutResult(t, func() error {
		return cmdResources([]string{"request", "weather", "--name", "Weather API", "--base-url", "https://weather.example", "--auth-mode", "api_key", "--header", "X-API-Key"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 1 || requestCalls != 1 || !strings.Contains(output, "setup_required") || !strings.Contains(output, resourceID) {
		t.Fatalf("calls=%d/%d output=%s", createCalls, requestCalls, output)
	}
}

func TestHostedResourceCallDecodesBoundedBrokerResponse(t *testing.T) {
	const (
		sessionID = "11111111-1111-1111-1111-111111111111"
		agentID   = "22222222-2222-2222-2222-222222222222"
		requestID = "33333333-3333-3333-3333-333333333333"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/developer-sessions/"+sessionID+"/resources/"+requestID+"/call" {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["method"] != "POST" || request["path"] != "/forecast" || request["body"] != `{"city":"Oslo"}` {
			t.Errorf("request = %#v", request)
		}
		fmt.Fprint(w, `{"statusCode":200,"headers":{"Content-Type":"application/json"},"body":"eyJvayI6dHJ1ZX0="}`)
	}))
	defer srv.Close()
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", sessionID)
	t.Setenv("AIRLOCK_API_URL", srv.URL)
	t.Setenv("AIRLOCK_AGENT_ID", agentID)
	t.Setenv("AIRLOCK_TOKEN", "developer-token")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	output, err := captureCommandStdoutResult(t, func() error {
		return cmdResources([]string{"call", requestID, "--method", "post", "--path", "/forecast", "--body", `{"city":"Oslo"}`})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "HTTP 200") || !strings.Contains(output, `{"ok":true}`) {
		t.Fatalf("output = %q", output)
	}
}

func TestResourceRequestNeverAcceptsSecretFlag(t *testing.T) {
	_, _, err := parseResourceFlags([]string{"--token", "secret"})
	if err == nil || !strings.Contains(err.Error(), "unknown resources flag") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureDeveloperSessionCreatesAndCachesLocalSession(t *testing.T) {
	const (
		agentID   = "11111111-1111-1111-1111-111111111111"
		sessionID = "22222222-2222-2222-2222-222222222222"
	)
	var gets, creates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/developer-sessions":
			gets++
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/developer-sessions":
			creates++
			fmt.Fprintf(w, `{"session":{"id":%q}}`, sessionID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	t.Chdir(dir)
	binding := agentBinding{}
	binding.putRemote("prod", agentRemoteBinding{AirlockURL: srv.URL, AgentID: agentID, Slug: "todo"})
	if err := writeAgentBinding(".", binding); err != nil {
		t.Fatal(err)
	}
	target := integrationTarget{baseURL: srv.URL, agentID: agentID, token: "token"}
	for range 2 {
		got, err := ensureDeveloperSession(t.Context(), target, resourceFlags{target: integrationTargetFlags{remote: "prod"}})
		if err != nil || got != sessionID {
			t.Fatalf("ensureDeveloperSession() = %q, %v", got, err)
		}
	}
	if gets != 1 || creates != 1 {
		t.Fatalf("gets=%d creates=%d", gets, creates)
	}
	stored, _, err := loadAgentBinding(".")
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := stored.remote("prod")
	if remote.DeveloperSession != sessionID {
		t.Fatalf("developer session = %q", remote.DeveloperSession)
	}
}
