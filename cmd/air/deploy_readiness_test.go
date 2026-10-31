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
		{"candidate", `{"deployment":{"configurationReadiness":{"candidateDeclarationsAvailable":true,"unboundResources":["connection/crm"],"unsetEnvVars":["REGION"],"unsetSecrets":["API_KEY"]}}}`, 200, []string{"Unbound resources: connection/crm", "Unset required environment values: REGION", "Unset required secrets: API_KEY", "do not block deployment", "only after the target deployment exists", "settings=environment", "target=production"}},
		{"current declarations", `{"deployment":{"configurationReadiness":{}}}`, 200, []string{"current target declarations"}},
		{"unavailable host", `{"deployment":{}}`, 200, []string{"readiness is unavailable", "settings=environment"}},
		{"inspection error", `{"error":"do-not-print-private-server-data"}`, 500, []string{"submission is unchanged", "Environment settings:"}},
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
			target := selectedDeployment{baseURL: server.URL, app: agentRemoteBinding{AgentID: "app"}, flags: targetFlags{target: "production"}, info: &airlockv1.DeploymentInfo{Id: "runtime", Kind: "production", WorkspaceUrl: server.URL + "/chat?app=app"}}
			output := captureCommandStderr(t, func() error { reportDeploymentReadiness(context.Background(), target, "candidate"); return nil })
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q: %s", want, output)
				}
			}
			if requests != 1 || strings.Contains(output, "do-not-print-private-server-data") {
				t.Fatal("diagnostic retried an operation or exposed private data")
			}
		})
	}
}
