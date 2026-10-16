package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestOperatorCommandsPinResolvedPreviewAndRevision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/"
	invocations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("unexpected operator credentials")
		}
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, testBuildAgentID)
		case prefix + previewRuntimeID, prefix + previewName:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q,"sourceRevisionId":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID, testSourceRevision)
		case prefix + previewName + "/http/report", prefix + previewName + "/tools/report/invoke", prefix + previewName + "/agents/summarize/runs":
			invocations++
			if r.Header.Get(wire.ExpectedSourceRevisionHeader) != testSourceRevision {
				t.Error("execution lacks atomic revision ceiling")
			}
			switch r.URL.Path {
			case prefix + previewName + "/http/report":
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.RawQuery != "period=week" || string(body) != "payload" {
					t.Error("HTTP method/query/body lost")
				}
				fmt.Fprint(w, "report")
			case prefix + previewName + "/tools/report/invoke":
				fmt.Fprintf(w, `{"runId":%q,"resultJson":"{}"}`, previewTaskID)
			default:
				fmt.Fprintf(w, `{"run":{"id":%q,"definition":"summarize","status":"queued"},"created":true}`, previewTaskID)
			}
		default:
			t.Errorf("unexpected target API %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b := agentBinding{}
	b.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"request", "POST", "/report?period=week", "--data", "payload"},
		{"tools", "invoke", "report", "--input", `{"period":"week"}`},
		{"agent", "start", "summarize", "--request-id", previewTaskID},
	} {
		command := append(args, "--dir", dir, "--target", "preview")
		if _, err := captureCommandStdoutResult(t, func() error { return run(command) }); err != nil {
			t.Fatal(err)
		}
	}
	if invocations != 3 {
		t.Fatalf("invocations=%d", invocations)
	}
}
