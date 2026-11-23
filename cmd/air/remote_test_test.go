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
		return cmdTest([]string{"--remote", "corp", "--agent", testBuildAgentID, "--target", "preview", "--revision", testSourceRevision, "--suite", "integration", "--dir", dir, "--wait"})
	})
	if err == nil || !strings.Contains(err.Error(), "fixture assertion") || posts != 1 || !strings.Contains(output, `"exitCode":1`) {
		t.Fatalf("output=%s error=%v requests=%d", output, err, posts)
	}
}

func TestRemoteTestJSONSnapshotAdmissionAndWait(t *testing.T) {
	for _, test := range []struct {
		command, want string
		wait          bool
	}{
		{"status", "completed", false}, {"status", "failed", true},
		{"run", "accepted", false}, {"run", "failed", true},
		{"cancel", "completed", false}, {"cancel", "completed", true},
	} {
		t.Run(fmt.Sprintf("%s/wait=%t", test.command, test.wait), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			dir := t.TempDir()
			posts, cancels := 0, 0
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("test lost admitted user credentials")
				}
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"candidate"}}`, testBuildAgentID)
				case prefix, "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewRuntimeID:
					fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"name":%q,"kind":"personal_preview","sourceRevisionId":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, testSourceRevision)
				case prefix + "/tests", prefix + "/tests/" + previewTaskID:
					status := "failed"
					if r.Method == http.MethodPost {
						posts++
					}
					if r.Method == http.MethodDelete {
						cancels++
						status = "cancelled"
					}
					if r.Header.Get(wire.ExpectedSourceRevisionHeader) != testSourceRevision {
						t.Error("test lost exact revision fence")
					}
					fmt.Fprintf(w, `{"test":{"id":%q,"appId":%q,"deploymentId":%q,"sourceRevisionId":%q,"suite":"integration","status":%q,"exitCode":1,"error":"fixture assertion"}}`, previewTaskID, testBuildAgentID, previewRuntimeID, testSourceRevision, status)
				default:
					t.Errorf("unexpected test upload/replay: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			binding := agentBinding{}
			binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			args := []string{"test"}
			if test.command != "run" {
				args = append(args, test.command, "--run", previewTaskID)
			}
			args = append(args, "--dir", dir, "--target", "preview", "--revision", testSourceRevision, "--json")
			if test.wait {
				args = append(args, "--wait")
			}
			output, err := invokeFixture(t, args...)
			result := decodeResult(t, output)
			if result.Status != test.want || (err != nil) != (test.want == "failed") || (test.command == "run" && posts != 1) || (test.command != "run" && posts != 0) || (test.command == "cancel" && cancels != 1) || (test.command != "cancel" && cancels != 0) {
				t.Fatalf("test outcome/replay: %v posts=%d cancels=%d %s", err, posts, cancels, output)
			}
			if !strings.Contains(output, "fixture assertion") || !strings.Contains(output, testSourceRevision) || !strings.Contains(output, previewTaskID) {
				t.Fatalf("lost test state/identity: %s", output)
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}
