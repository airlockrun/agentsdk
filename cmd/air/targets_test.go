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

func TestTargetStatusJSONIncludesEnvironmentRevisionsOnly(t *testing.T) {
	applied := int64(7)
	s := selectedDeployment{flags: targetFlags{json: true}, remoteName: "origin", info: &airlockv1.DeploymentInfo{Name: "production", DesiredEnvRevision: 8, AppliedEnvRevision: &applied, EnvConfigurationState: "restart_required", RestartRequired: true}}
	output := captureCommandStdout(t, s.print)
	var result struct {
		Deployment struct {
			Desired string `json:"desiredEnvRevision"`
			Applied string `json:"appliedEnvRevision"`
			State   string `json:"envConfigurationState"`
			Restart bool   `json:"restartRequired"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Deployment.Desired != "8" || result.Deployment.Applied != "7" || result.Deployment.State != "restart_required" || !result.Deployment.Restart {
		t.Fatalf("missing environment status: %s", output)
	}
	if strings.Contains(output, "envStartAttempt") || strings.Contains(output, "values") {
		t.Fatal("status exposes private startup data")
	}
}

func TestHostedDeploymentUsesInjectedConcreteIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
	t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
	t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
	t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
	t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
	t.Setenv("AIRLOCK_TOKEN", testAccessToken)
	owner, runtime := previewWorkspaceID, previewRuntimeID
	denyProduction := false
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Errorf("incorrect hosted request: %s %s", r.Method, r.URL)
		}
		if r.URL.Path == "/api/v1/agents/"+testBuildAgentID+"/deployments/production" {
			if denyProduction {
				http.Error(w, "host permission denied", http.StatusForbidden)
				return
			}
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"production","name":"production"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID)
			return
		}
		fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q}}`, runtime, runtime, testBuildAgentID, previewName, owner)
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	f := targetFlags{dir: t.TempDir(), target: "preview"}
	s, err := resolveDeployment(t.Context(), f, true)
	if err != nil || s.selector != previewName || s.workspaceID != previewWorkspaceID || s.baseURL != server.URL || s.remoteName != "hosted" || s.token != testAccessToken {
		t.Fatalf("hosted resolution: %+v %v", s, err)
	}
	for _, mutate := range []func(*targetFlags){
		func(f *targetFlags) { f.remote = "corp" },
		func(f *targetFlags) { f.agent = previewRuntimeID },
		func(f *targetFlags) { f.url = "https://other.example" },
	} {
		conflict := f
		mutate(&conflict)
		before := requests
		if _, err := resolveDeployment(t.Context(), conflict, false); err == nil || requests != before {
			t.Fatal("conflicting hosted flags reached target")
		}
	}
	for _, remote := range []agentRemoteBinding{
		{AirlockURL: "https://other.example", AgentID: testBuildAgentID},
		{AirlockURL: server.URL, AgentID: previewRuntimeID},
	} {
		conflict := f
		conflict.dir = t.TempDir()
		b := agentBinding{}
		b.putRemote("hosted", remote)
		if err := writeAgentBinding(conflict.dir, b); err != nil {
			t.Fatal(err)
		}
		before := requests
		if _, err := resolveDeployment(t.Context(), conflict, false); err == nil || requests != before {
			t.Fatal("conflicting hosted local binding reached target")
		}
	}
	production := f
	production.target = "production"
	if s, err := resolveDeployment(t.Context(), production, false); err != nil || s.info.Name != "production" {
		t.Fatalf("explicit production must reach host authorization: %+v %v", s, err)
	}
	denyProduction = true
	if _, err := resolveDeployment(t.Context(), production, false); !hasHTTPStatus(err, http.StatusForbidden) {
		t.Fatalf("host authorization not preserved: %v", err)
	}
	denyProduction = false
	implicit := f
	implicit.target = ""
	if s, err := resolveDeployment(t.Context(), implicit, false); err != nil || s.selector != previewName {
		t.Fatalf("hosted default: %+v %v", s, err)
	}
	alias := f
	alias.dir = t.TempDir()
	alias.remote = "corp"
	b := agentBinding{}
	b.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID})
	if err := writeAgentBinding(alias.dir, b); err != nil {
		t.Fatal(err)
	}
	if s, err := resolveDeployment(t.Context(), alias, false); err != nil || s.remoteName != "corp" || s.info.Id != previewRuntimeID {
		t.Fatalf("consistent remote alias rejected: %+v %v", s, err)
	}
	owner = previewTaskID
	if _, err := resolveDeployment(t.Context(), f, false); err == nil {
		t.Fatal("hosted owner mismatch accepted")
	}
	owner, runtime = previewWorkspaceID, previewTaskID
	if _, err := resolveDeployment(t.Context(), f, false); err == nil {
		t.Fatal("hosted runtime mismatch accepted")
	}
	t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", "preview")
	before := requests
	if _, err := resolveDeployment(t.Context(), f, false); err == nil || requests != before {
		t.Fatal("ambiguous injected alias accepted")
	}
}

func TestTargetAppOutput(t *testing.T) {
	for _, workspaceURL := range []string{"https://dashboard.example.test/chat?app=" + testBuildAgentID, "https://dashboard.example.test/chat/" + previewWorkspaceID, ""} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", workspaceURL, asJSON), func(t *testing.T) {
				s := selectedDeployment{flags: targetFlags{json: asJSON}, token: "secret-not-for-output", info: &airlockv1.DeploymentInfo{Id: previewRuntimeID, Name: previewName, WorkspaceUrl: workspaceURL, RouteBaseUrl: "https://runtime.example.test"}}
				out := captureCommandStdout(t, s.print)
				if strings.Contains(out, s.token) {
					t.Fatal("credential printed")
				}
				if asJSON {
					var result struct {
						Deployment struct {
							WorkspaceURL string `json:"workspaceUrl"`
							RouteBaseURL string `json:"routeBaseUrl"`
						} `json:"deployment"`
					}
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatal(err)
					}
					if result.Deployment.WorkspaceURL != workspaceURL || result.Deployment.RouteBaseURL != s.info.RouteBaseUrl {
						t.Fatalf("JSON: %s", out)
					}
				} else {
					if !strings.Contains(out, "App URL: "+s.info.RouteBaseUrl) || strings.Contains(out, "Workspace URL:") {
						t.Fatalf("text: %s", out)
					}
					if workspaceURL != "" && strings.Contains(out, workspaceURL) {
						t.Fatalf("workspace URL printed instead of app URL: %s", out)
					}
				}
			})
		}
	}
}

func TestTargetAppURLUsesSelectedTarget(t *testing.T) {
	for _, tt := range []struct{ name, kind, appURL string }{
		{"production", "production", "https://server-production.example.test"},
		{previewName, "personal_preview", "https://server-exact-preview.example.test"},
		{"unavailable", "personal_preview", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := selectedDeployment{info: &airlockv1.DeploymentInfo{
				Name: tt.name, Kind: tt.kind, RouteBaseUrl: tt.appURL,
				WorkspaceUrl: "https://dashboard.example.test/chat/" + previewWorkspaceID,
			}}
			out := captureCommandStdout(t, func() error { s.printAppURL(); return nil })
			want := "App URL: " + tt.appURL + "\n"
			if tt.appURL == "" {
				want = "App URL: unavailable (server returned no route URL for this target)\n"
			}
			if out != want {
				t.Fatalf("output=%q want=%q", out, want)
			}
		})
	}
}

func TestHostedStatusUsesServerAppURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
	t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
	t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
	t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
	t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
	t.Setenv("AIRLOCK_TOKEN", testAccessToken)
	workspaceURL := "https://public.example.test/prefix%20name/chat/22222222-2222-4222-8222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testAccessToken || r.URL.Path != "/api/v1/agents/"+testBuildAgentID+"/deployments/"+previewName {
			t.Errorf("incorrect authenticated target: %s %s", r.Method, r.URL)
		}
		fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q,"workspaceUrl":%q,"routeBaseUrl":"https://runtime.example.test"}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID, workspaceURL)
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	for _, args := range [][]string{{"--dir", t.TempDir()}, {"--dir", t.TempDir(), "--json"}} {
		out := captureCommandStdout(t, func() error { return cmdTargetStatus(args) })
		if !strings.Contains(out, "https://runtime.example.test") || strings.Contains(out, testAccessToken) {
			t.Fatalf("status: %s", out)
		}
	}
}
