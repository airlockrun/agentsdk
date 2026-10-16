package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestRemoteSuiteCommandUsesExistingCandidate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewRuntimeID
	concretePrefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("unexpected test credentials")
		}
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"candidate"}}`, testBuildAgentID)
		case prefix, concretePrefix:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"name":%q,"kind":"personal_preview","ownerUserId":%q,"ownerWorkspaceId":%q,"sourceRevisionId":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewOwnerID, previewWorkspaceID, testSourceRevision)
		case concretePrefix + "/tests":
			posts++
			if r.Method != http.MethodPost || r.Header.Get(wire.ExpectedSourceRevisionHeader) != testSourceRevision {
				t.Error("test admission lacks exact revision/method")
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["sourceRevisionId"] != testSourceRevision || request["suite"] != "integration" {
				t.Errorf("request=%v", request)
			}
			fmt.Fprintf(w, `{"test":{"id":%q,"appId":%q,"deploymentId":%q,"sourceRevisionId":%q,"suite":"integration","status":"failed","exitCode":1,"error":"fixture assertion"}}`, previewTaskID, testBuildAgentID, previewRuntimeID, testSourceRevision)
		default:
			t.Errorf("unexpected upload/fixture/other route %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bind := agentBinding{}
	bind.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
	if err := writeAgentBinding(dir, bind); err != nil {
		t.Fatal(err)
	}
	output, err := captureCommandStdoutResult(t, func() error {
		return cmdTest([]string{"--remote", "corp", "--agent", testBuildAgentID, "--target", "preview", "--revision", testSourceRevision, "--suite", "integration", "--dir", dir})
	})
	if err == nil || !strings.Contains(err.Error(), "fixture assertion") || posts != 1 || !strings.Contains(output, `"exitCode":1`) {
		t.Fatalf("output=%s error=%v requests=%d", output, err, posts)
	}
}
