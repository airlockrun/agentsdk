package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResourceBindingCLI(t *testing.T) {
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	const resourceID = "11111111-1111-4111-8111-111111111111"
	for _, typ := range []string{"connection", "mcp_server"} {
		t.Run(typ, func(t *testing.T) {
			bound := false
			root := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("binding lost operator credential")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, testBuildAgentID)
				case "GET " + root:
					fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"name":"production","kind":"production"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID)
				case "GET " + root + "/bindings":
					fmt.Fprintf(w, `{"runtimeAgentId":%q,"bindings":[{"type":"connection","slug":"provider_slot"},{"type":"mcp_server","slug":"provider_slot"}]}`, testBuildAgentID)
				case "GET /api/v1/resources/resolve/production":
					fmt.Fprintf(w, `{"id":%q,"type":%q}`, resourceID, typ)
				case "PUT " + root + "/bindings/" + typ + "/provider_slot":
					var input struct {
						ResourceID string `json:"resourceId"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ResourceID != resourceID {
						t.Errorf("binding payload = %+v, error = %v", input, err)
					}
					bound = true
					fmt.Fprintf(w, `{"runtimeAgentId":%q}`, testBuildAgentID)
				default:
					t.Errorf("unexpected resource route: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			args := []string{"bind", "production/provider_slot", "--resource", "/production", "--url", server.URL, "--agent", testBuildAgentID}
			if err := cmdResources(args); err == nil || !strings.Contains(err.Error(), "multiple resource types") {
				t.Fatalf("ambiguous slot: %v", err)
			}
			if bound {
				t.Fatal("ambiguous slot mutated binding")
			}
			if err := cmdResources(append(args, "--type", typ)); err != nil {
				t.Fatal(err)
			}
			if !bound {
				t.Fatal("binding PUT not sent")
			}
		})
	}
}

func TestResourceCLIRejectsInvalidSelection(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"bare", []string{"inspect", "provider"}, "bare names are ambiguous"},
		{"extra segment", []string{"bind", "preview/provider/extra"}, "resource address requires"},
		{"target conflict", []string{"bind", "preview/provider", "--target", "production"}, "conflicts with --target"},
		{"list conflict", []string{"list", "preview", "--target", "production"}, "conflicts with --target"},
		{"global target", []string{"inspect", "/production", "--target", "preview"}, "user+remote scoped"},
		{"candidate flag", []string{"bind", "preview/provider", "--revision", "revision"}, "not supported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := cmdResources(tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveResourceRemoteHosted(t *testing.T) {
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "workspace")
	t.Setenv("AIRLOCK_API_URL", "https://admitted.example")
	t.Setenv("AIRLOCK_TOKEN", "command-execution-token")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("same", agentRemoteBinding{AirlockURL: "https://admitted.example", AgentID: "unrelated-app-not-selected"})
	binding.putRemote("foreign", agentRemoteBinding{AirlockURL: "https://other.example"})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, remote, url string
		denied            bool
	}{
		{name: "injected host without a saved remote"},
		{name: "matching remote", remote: "same"},
		{name: "matching explicit host", url: "https://admitted.example"},
		{name: "foreign remote", remote: "foreign", denied: true},
		{name: "foreign URL", url: "https://other.example", denied: true},
		{name: "unknown remote", remote: "missing", denied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, token, err := resolveUserRemote(t.Context(), targetFlags{dir: dir, remote: tt.remote, url: tt.url})
			if (err != nil) != tt.denied {
				t.Fatalf("error=%v", err)
			}
			if tt.denied {
				if token != "" {
					t.Fatal("denied remote returned a credential")
				}
				return
			}
			if base != "https://admitted.example" || token != "command-execution-token" {
				t.Fatal("resource selection lost injected host/credential")
			}
		})
	}
	t.Setenv("AIRLOCK_TOKEN", "")
	if _, _, err := resolveUserRemote(t.Context(), targetFlags{dir: dir}); err == nil {
		t.Fatal("missing execution credential accepted")
	}
}

func TestHostedResourceRemoteDoesNotForwardCredential(t *testing.T) {
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "workspace")
	t.Setenv("AIRLOCK_API_URL", "https://admitted.example")
	t.Setenv("AIRLOCK_TOKEN", "command-execution-token")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Chdir(t.TempDir())
	called := false
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer foreign.Close()
	if err := cmdResources([]string{"list", "--url", foreign.URL}); err == nil {
		t.Fatal("foreign host accepted")
	}
	if called {
		t.Fatal("injected credential sent to foreign host")
	}
	for _, flag := range []string{"--agent", "--target"} {
		if err := cmdResources([]string{"inspect", "/personal-resource", flag, "preview"}); err == nil || !strings.Contains(err.Error(), "user+remote scoped") {
			t.Fatalf("%s: %v", flag, err)
		}
	}
}

func TestResolveResourceRemoteLogin(t *testing.T) {
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := saveLoginCredentials("https://operator.example", "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("operator", agentRemoteBinding{AirlockURL: "https://operator.example"})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	base, token, err := resolveUserRemote(t.Context(), targetFlags{dir: dir, remote: "operator"})
	if err != nil || base != "https://operator.example" || token != testAccessToken {
		t.Fatalf("login resource selection failed: %v", err)
	}
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "build-token")
	if _, _, err := resolveUserRemote(t.Context(), targetFlags{dir: dir, remote: "operator"}); err == nil {
		t.Fatal("build token accessed user resource inventory")
	}
}

func TestResourceOperatorCommands(t *testing.T) {
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tt := range []struct {
		name, method, path, response string
		args                         []string
	}{
		{"list", "GET", "/api/v1/resources", `{"resources":[]}`, []string{"list"}},
		{"inspect", "GET", "/api/v1/resources/connection/" + id, `{"id":"` + id + `","type":"connection"}`, []string{"inspect", "/" + id}},
		{"request", "POST", "/api/v1/resources/connections/" + id + "/request", `{"statusCode":200}`, []string{"request", "/" + id, "--path", "/v1/me"}},
		{"mcp-tools", "GET", "/api/v1/resources/mcp_servers/" + id + "/tools", `{"tools":[]}`, []string{"mcp-tools", "/" + id}},
		{"mcp-call", "POST", "/api/v1/resources/mcp_servers/" + id + "/call", `{"content":[]}`, []string{"mcp-call", "/" + id, "--tool", "lookup", "--data", `{"query":"airlock"}`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if strings.HasPrefix(r.URL.Path, "/api/v1/resources/resolve/") {
					typ := "connection"
					if strings.HasPrefix(tt.name, "mcp-") {
						typ = "mcp_server"
					}
					_, _ = w.Write([]byte(`{"id":"` + id + `","type":"` + typ + `"}`))
					return
				}
				if r.Method != tt.method || r.URL.Path != tt.path || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("operator resource route or authentication mismatch")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			if err := cmdResources(append(tt.args, "--url", server.URL)); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("resource command did not reach operator API")
			}
		})
	}
}
