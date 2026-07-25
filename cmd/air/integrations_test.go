package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/goai/mcp"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestMCPProbeAddressAllowed(t *testing.T) {
	tests := []struct {
		address       string
		allowLoopback bool
		want          bool
	}{
		{address: "8.8.8.8", want: true},
		{address: "127.0.0.1", want: false},
		{address: "127.0.0.1", allowLoopback: true, want: true},
		{address: "10.0.0.1", want: false},
		{address: "100.64.0.1", want: false},
		{address: "198.18.0.1", want: false},
		{address: "169.254.169.254", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			if got := mcpProbeAddressAllowed(net.ParseIP(tt.address), tt.allowLoopback); got != tt.want {
				t.Errorf("mcpProbeAddressAllowed(%s, %v) = %v, want %v", tt.address, tt.allowLoopback, got, tt.want)
			}
		})
	}
}

func TestResolveIntegrationTargetCodegen(t *testing.T) {
	t.Setenv("AIRLOCK_API_URL", "https://airlock.example/")
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "token")
	target, err := resolveIntegrationTarget(t.Context(), integrationTargetFlags{})
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
	if _, err := resolveIntegrationTarget(t.Context(), integrationTargetFlags{}); err == nil {
		t.Fatal("resolveIntegrationTarget() succeeded with incomplete environment")
	}
}

func TestResolveIntegrationTargetSelectsNamedRemote(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const prodID = "11111111-1111-1111-1111-111111111111"
	const devID = "22222222-2222-2222-2222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/"+devID {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent":{"id":"` + devID + `","slug":"dev"}}`))
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "dev@example.com", "user-token", ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("prod", agentRemoteBinding{AirlockURL: server.URL, AgentID: prodID, Slug: "prod"})
	binding.putRemote("dev", agentRemoteBinding{AirlockURL: server.URL, AgentID: devID, Slug: "dev"})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	target, err := resolveIntegrationTarget(t.Context(), integrationTargetFlags{remote: "dev"})
	if err != nil {
		t.Fatalf("resolveIntegrationTarget: %v", err)
	}
	if target.baseURL != server.URL || target.agentID != devID || target.token != "user-token" || target.codegen {
		t.Fatalf("target = %+v", target)
	}
	got, _, err := loadAgentBinding(".")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultRemote != "prod" {
		t.Fatalf("DefaultRemote = %q, want prod", got.DefaultRemote)
	}
}

func TestResolveIntegrationTargetRejectsRemoteURLChange(t *testing.T) {
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("prod", agentRemoteBinding{AirlockURL: "https://prod.example", AgentID: "agent"})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	_, err := resolveIntegrationTarget(t.Context(), integrationTargetFlags{url: "https://dev.example"})
	if err == nil || !strings.Contains(err.Error(), "different --remote") {
		t.Fatalf("resolveIntegrationTarget error = %v", err)
	}
}

func TestResolveIntegrationTargetRejectsCodegenSelectors(t *testing.T) {
	t.Setenv("AIRLOCK_API_URL", "https://airlock.example")
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "token")
	_, err := resolveIntegrationTarget(t.Context(), integrationTargetFlags{remote: "dev"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("resolveIntegrationTarget error = %v", err)
	}
}

func TestParseIntegrationTargetFlags(t *testing.T) {
	flags, err := parseIntegrationTargetFlags([]string{"--remote", "dev", "--url", "https://airlock.example", "--agent", "dev-agent"})
	if err != nil {
		t.Fatalf("parseIntegrationTargetFlags: %v", err)
	}
	if flags.remote != "dev" || flags.url != "https://airlock.example" || flags.agent != "dev-agent" {
		t.Fatalf("flags = %#v", flags)
	}
	if _, err := parseIntegrationTargetFlags([]string{"--remote", "bad remote"}); err == nil {
		t.Fatal("parseIntegrationTargetFlags accepted an invalid remote")
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

func TestExecNamedRemotePreservesCommandFlags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const prodID = "11111111-1111-1111-1111-111111111111"
	const devID = "22222222-2222-2222-2222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agents/" + devID:
			encoded, _ := protojson.Marshal(&airlockv1.GetAgentDetailResponse{Agent: &airlockv1.AgentInfo{Id: devID, Slug: "dev"}})
			_, _ = w.Write(encoded)
		case "/api/v1/agents/" + devID + "/integrations/exec/shell/run":
			var req airlockv1.InvokeExecRequest
			raw, _ := io.ReadAll(r.Body)
			if err := protojson.Unmarshal(raw, &req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Command != "runner" || len(req.Args) != 2 || req.Args[0] != "--remote" || req.Args[1] != "inside" {
				t.Errorf("command = %q, args = %q", req.Command, req.Args)
			}
			encoded, _ := protojson.Marshal(&airlockv1.InvokeExecResponse{Stdout: []byte("ok")})
			_, _ = w.Write(encoded)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "dev@example.com", "user-token", ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("prod", agentRemoteBinding{AirlockURL: server.URL, AgentID: prodID, Slug: "prod"})
	binding.putRemote("dev", agentRemoteBinding{AirlockURL: server.URL, AgentID: devID, Slug: "dev"})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	output := captureCommandStdout(t, func() error {
		return cmdExec([]string{"run", "shell", "--remote", "dev", "--", "runner", "--remote", "inside"})
	})
	if output != "ok" {
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

func TestProbeMCPReportsOAuthMode(t *testing.T) {
	tests := []struct {
		name                 string
		oauthMetadata        bool
		registrationEndpoint bool
		mcpUnauthorized      bool
		wantMCPStatus        string
		wantToolCount        int
		wantStatus           string
		wantDCR              string
		wantMode             string
		wantUnavailable      string
		wantMessage          string
	}{
		{
			name:                 "DCR advertised",
			oauthMetadata:        true,
			registrationEndpoint: true,
			wantMCPStatus:        "connected",
			wantToolCount:        1,
			wantStatus:           "oauth_metadata_discovered",
			wantDCR:              "advertised",
			wantMode:             "MCPAuthOAuthDiscovery",
			wantMessage:          "registration was not attempted",
		},
		{
			name:            "DCR not advertised",
			oauthMetadata:   true,
			wantMCPStatus:   "connected",
			wantToolCount:   1,
			wantStatus:      "oauth_metadata_discovered",
			wantDCR:         "not_advertised",
			wantMode:        "MCPAuthOAuth",
			wantUnavailable: "MCPAuthOAuthDiscovery",
			wantMessage:     "does not advertise a dynamic client registration endpoint",
		},
		{
			name:          "auth unknown",
			wantMCPStatus: "connected",
			wantToolCount: 1,
			wantStatus:    "unknown",
			wantDCR:       "unknown",
			wantMode:      "unknown",
			wantMessage:   "authentication mode is unknown",
		},
		{
			name:            "authentication required",
			oauthMetadata:   true,
			mcpUnauthorized: true,
			wantMCPStatus:   "authentication_required",
			wantToolCount:   0,
			wantStatus:      "oauth_metadata_discovered",
			wantDCR:         "not_advertised",
			wantMode:        "MCPAuthOAuth",
			wantUnavailable: "MCPAuthOAuthDiscovery",
			wantMessage:     "does not advertise a dynamic client registration endpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var registrationRequests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/oauth-protected-resource/mcp/v1":
					if r.Method != http.MethodGet {
						t.Errorf("protected resource metadata method = %s", r.Method)
					}
					if !tt.oauthMetadata {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(mcp.OAuthProtectedResourceMetadata{
						Resource:             server.URL + "/mcp/v1",
						AuthorizationServers: []string{server.URL + "/"},
						ScopesSupported:      []string{"mail.read"},
					})
					return
				case "/.well-known/oauth-protected-resource":
					http.NotFound(w, r)
					return
				case "/.well-known/oauth-authorization-server":
					if r.Method != http.MethodGet {
						t.Errorf("authorization server metadata method = %s", r.Method)
					}
					w.Header().Set("Content-Type", "application/json")
					metadata := mcp.AuthorizationServerMetadata{
						Issuer:                        server.URL,
						AuthorizationEndpoint:         server.URL + "/authorize",
						TokenEndpoint:                 server.URL + "/token",
						CodeChallengeMethodsSupported: []string{"S256"},
					}
					if tt.registrationEndpoint {
						metadata.RegistrationEndpoint = server.URL + "/register"
					}
					_ = json.NewEncoder(w).Encode(metadata)
					return
				case "/register":
					registrationRequests.Add(1)
					http.Error(w, "registration must not be attempted", http.StatusInternalServerError)
					return
				case "/mcp/v1":
					serveProbeMCP(w, r, tt.mcpUnauthorized)
					return
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			output := captureCommandStdout(t, func() error { return probeMCP(server.URL + "/mcp/v1") })
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(output), &envelope); err != nil {
				t.Fatalf("decode probe envelope: %v\n%s", err, output)
			}
			for _, field := range []string{"url", "mcpStatus", "tools", "auth"} {
				if _, ok := envelope[field]; !ok {
					t.Errorf("probe output missing %q", field)
				}
			}
			var authFields map[string]json.RawMessage
			if err := json.Unmarshal(envelope["auth"], &authFields); err != nil {
				t.Fatalf("decode auth envelope: %v", err)
			}
			for _, field := range []string{"status", "dynamicClientRegistration", "likelyAuthMode", "message"} {
				if _, ok := authFields[field]; !ok {
					t.Errorf("auth output missing %q", field)
				}
			}
			var result mcpProbeResult
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatalf("decode probe output: %v\n%s", err, output)
			}
			if result.MCPStatus != tt.wantMCPStatus {
				t.Errorf("MCPStatus = %q, want %q", result.MCPStatus, tt.wantMCPStatus)
			}
			if len(result.Tools) != tt.wantToolCount || (tt.wantToolCount > 0 && result.Tools[0].Name != "lookup") {
				t.Fatalf("Tools = %#v", result.Tools)
			}
			if result.Auth.Status != tt.wantStatus || result.Auth.DynamicClientRegistration != tt.wantDCR || result.Auth.LikelyAuthMode != tt.wantMode {
				t.Errorf("Auth = %#v", result.Auth)
			}
			if tt.wantUnavailable != "" && (len(result.Auth.UnavailableAuthModes) != 1 || result.Auth.UnavailableAuthModes[0] != tt.wantUnavailable) {
				t.Errorf("UnavailableAuthModes = %v", result.Auth.UnavailableAuthModes)
			}
			if !strings.Contains(result.Auth.Message, tt.wantMessage) {
				t.Errorf("Message = %q, want substring %q", result.Auth.Message, tt.wantMessage)
			}
			if registrationRequests.Load() != 0 {
				t.Errorf("registration requests = %d, want 0", registrationRequests.Load())
			}
		})
	}
}

func serveProbeMCP(w http.ResponseWriter, r *http.Request, unauthorized bool) {
	if unauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	results := map[string]any{
		"initialize": map[string]any{
			"protocolVersion": mcp.LatestProtocolVersion,
			"instructions":    "Use carefully.",
		},
		"tools/list": map[string]any{
			"tools": []map[string]any{{
				"name": "lookup", "description": "Look up a value", "inputSchema": map[string]any{"type": "object"},
			}},
		},
		"resources/list": map[string]any{"resources": []any{}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": results[request.Method]})
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
