package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

func TestDeploymentReadinessIsInformationalAndTargetSpecific(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       []string
	}{
		{"candidate", `{"deployment":{"id":"runtime","appId":"app","runtimeAgentId":"runtime","configurationReadiness":{"candidateDeclarationsAvailable":true,"unboundResources":["connection/crm"],"unsetEnvVars":["REGION"],"unsetSecrets":["API_KEY"]}}}`, 200, []string{"Candidate", "Unbound resources: connection/crm", "Unset required environment values: REGION", "Unset required secrets: API_KEY", "settings=environment", "settings=main", "target=production"}},
		{"current declarations", `{"deployment":{"id":"runtime","appId":"app","runtimeAgentId":"runtime","configurationReadiness":{}}}`, 200, nil},
		{"current missing configuration", `{"deployment":{"id":"runtime","appId":"app","runtimeAgentId":"runtime","configurationReadiness":{"unboundResources":["connection/current-crm"]}}}`, 200, []string{"Current target (candidate declarations unavailable)", "connection/current-crm"}},
		{"unavailable host", `{"deployment":{"id":"runtime","appId":"app","runtimeAgentId":"runtime"}}`, 200, nil},
		{"wrong identity", `{"deployment":{"id":"different","appId":"app","runtimeAgentId":"different","configurationReadiness":{"unboundResources":["must-not-print-other-target-slot"]}}}`, 200, []string{"inconsistent deployment identity", "submission is unchanged"}},
		{"inspection error", `{"error":"do-not-print-private-server-data"}`, 500, []string{"submission is unchanged"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents/app/deployments/production" || r.URL.Query().Get("candidate_revision_id") != "candidate" {
					t.Errorf("unexpected diagnostic request: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			target := selectedDeployment{baseURL: server.URL, remoteName: "corp", app: agentRemoteBinding{AgentID: "app"}, flags: targetFlags{dir: ".", target: "production"}, info: &airlockv1.DeploymentInfo{Id: "runtime", Name: "production", Kind: "production", WorkspaceUrl: server.URL + "/chat?app=app"}}
			output := captureCommandStderr(t, func() error { reportDeploymentReadiness(context.Background(), target, "candidate"); return nil })
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q: %s", want, output)
				}
			}
			if requests != 1 || strings.Contains(output, "do-not-print-private-server-data") || strings.Contains(output, "must-not-print-other-target-slot") {
				t.Fatal("diagnostic retried an operation or exposed private data")
			}
			if len(tt.want) == 0 && output != "" {
				t.Fatalf("unavailable declarations are not a configuration warning: %q", output)
			}
			if strings.Contains(output, "only after the target deployment exists") || strings.Contains(output, "#connections") {
				t.Fatalf("inaccurate lifecycle or unsupported browser route: %s", output)
			}
		})
	}
}
