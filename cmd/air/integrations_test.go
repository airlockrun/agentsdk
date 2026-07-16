package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestResolveIntegrationTargetCodegen(t *testing.T) {
	t.Setenv("AIRLOCK_API_URL", "https://airlock.example/")
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "token")
	target, err := resolveIntegrationTarget(t.Context())
	if err != nil {
		t.Fatalf("resolveIntegrationTarget() error: %v", err)
	}
	if target.baseURL != "https://airlock.example" || target.agentID != "agent-id" || target.token != "token" || !target.codegen {
		t.Fatalf("resolveIntegrationTarget() = %+v", target)
	}
	if got := target.path("/mcp/server/tools"); got != "/api/codegen/integrations/mcp/server/tools" {
		t.Fatalf("target.path() = %q", got)
	}
}

func TestResolveIntegrationTargetRequiresCompleteEnvironment(t *testing.T) {
	t.Setenv("AIRLOCK_API_URL", "https://airlock.example")
	if _, err := resolveIntegrationTarget(t.Context()); err == nil {
		t.Fatal("resolveIntegrationTarget() succeeded with incomplete environment")
	}
}

func TestConnectionRequestCodegen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/codegen/integrations/connections/home/request" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer integration-token" {
			t.Errorf("Authorization = %q", got)
		}
		var req airlockv1.InvokeConnectionRequest
		raw, _ := io.ReadAll(r.Body)
		if err := protojson.Unmarshal(raw, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Method != http.MethodPost || req.Path != "/devices" || string(req.Body) != `{"active":true}` {
			t.Errorf("request method/path/body = %q/%q/%q", req.Method, req.Path, req.Body)
		}
		encoded, _ := protojson.Marshal(&airlockv1.InvokeConnectionResponse{StatusCode: 200, Body: []byte(`{"ok":true}`)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "integration-token")

	output := captureCommandStdout(t, func() error {
		return cmdConnection([]string{"request", "home", "--method", "POST", "--path", "/devices", "--data", `{"active":true}`})
	})
	if output != `{"ok":true}` {
		t.Fatalf("stdout = %q", output)
	}
}

func TestDeployRejectsIntegrationToken(t *testing.T) {
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "integration-token")
	err := cmdDeploy(nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("cmdDeploy() error = %v", err)
	}
}

func TestConnectionRequestReturnsErrorForUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := protojson.Marshal(&airlockv1.InvokeConnectionResponse{StatusCode: http.StatusUnauthorized, Body: []byte("denied")})
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "integration-token")

	output, err := captureCommandStdoutResult(t, func() error {
		return cmdConnection([]string{"request", "home", "--path", "/devices"})
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("cmdConnection() error = %v", err)
	}
	if output != "denied" {
		t.Fatalf("stdout = %q", output)
	}
}

func captureCommandStdout(t *testing.T, run func() error) string {
	t.Helper()
	output, err := captureCommandStdoutResult(t, run)
	if err != nil {
		t.Fatalf("command error: %v", err)
	}
	return output
}

func captureCommandStdoutResult(t *testing.T, run func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	runErr := run()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), runErr
}
