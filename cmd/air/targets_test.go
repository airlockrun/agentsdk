package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
