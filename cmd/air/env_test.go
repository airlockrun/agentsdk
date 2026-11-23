package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
)

const envTestAgentID = "11111111-1111-1111-1111-111111111111"

func TestWorkspaceSettingsLinksUseBrowserURLOnly(t *testing.T) {
	for _, test := range []struct{ name, workspace, kind, want string }{
		{"environment", "https://public.example.test/chat/session?app=app", "production", "https://public.example.test/chat/session?app=app&settings=environment&target=production"},
		{"preview", "https://public.example.test/chat/session?app=app", "personal_preview", "https://public.example.test/chat/session?app=app&settings=environment&target=runtime"},
		{"absent", "", "production", ""},
		{"relative", "/chat/session", "production", ""},
		{"unsafe", "javascript:alert(1)", "production", ""},
		{"userinfo", "https://secret:password@public.example.test/chat", "production", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := selectedDeployment{baseURL: "http://internal-api:8080", info: &airlockv1.DeploymentInfo{Id: "runtime", Kind: test.kind, WorkspaceUrl: test.workspace}}
			if got := envSettingsURL(target); got != test.want || strings.Contains(got, "internal-api") {
				t.Fatalf("browser link: %q, want %q", got, test.want)
			}
		})
	}
}

func TestEnvJSONRedactsSecretValuesAndDefaults(t *testing.T) {
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvVarList(t, w,
			&airlockv1.EnvVarInfo{Slug: "API_KEY", IsSecret: true, Configured: true, Value: "secret-value-not-for-output", DefaultValue: "secret-default-not-for-output"},
			&airlockv1.EnvVarInfo{Slug: "REGION", Configured: true, Value: "eu-west"},
		)
	})
	defer server.Close()
	output, err := invokeFixture(t, "env", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, output)
	for _, secret := range []string{"secret-value-not-for-output", "secret-default-not-for-output", testAccessToken} {
		if strings.Contains(output, secret) {
			t.Fatalf("secret leaked: %s", output)
		}
	}
	if !strings.Contains(output, "eu-west") || !strings.Contains(output, "API_KEY") {
		t.Fatalf("lost non-secret configuration metadata: %s", output)
	}
}

func TestEnvJSONMutationDoesNotGuessActivationOrReplay(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			posts := 0
			server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "API_KEY", IsSecret: true})
					return
				}
				posts++
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"error":"fixture failure with current-private-value"}`))
				}
			})
			defer server.Close()
			output, err := invokeFixture(t, "env", "set", "API_KEY", "current-private-value", "--json")
			result := decodeResult(t, output)
			if posts != 1 || strings.Contains(output, "current-private-value") || strings.Contains(output, testAccessToken) {
				t.Fatalf("replayed configuration or leaked secret: %s", output)
			}
			if status == http.StatusNoContent {
				data := result.Data.(map[string]any)
				if err != nil || data["runtimeConfiguration"] != "unobserved" || data["restartRequired"] != nil || result.Operation.Admission != "accepted" {
					t.Fatalf("guessed runtime activation: %v %s", err, output)
				}
			} else if status == http.StatusForbidden {
				if err == nil || result.Error.Code != "permission_denied" || result.Operation.Admission != "blocked" || !strings.Contains(output, "[redacted]") {
					t.Fatalf("redaction lost permission denial: %v %s", err, output)
				}
			} else if err == nil || result.Error.Code != "effect_unknown" || result.Operation.Admission != "unknown" || !strings.Contains(output, "[redacted]") {
				t.Fatalf("uncertain configuration contract: %v %s", err, output)
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}

func TestCmdEnvList(t *testing.T) {
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents/"+envTestAgentID+"/env-vars" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeEnvVarList(t, w,
			&airlockv1.EnvVarInfo{Slug: "API_TOKEN", Description: "Private token", IsSecret: true, Configured: true},
			&airlockv1.EnvVarInfo{Slug: "GREETING", Description: "Greeting text", Configured: true, DefaultValue: "hello", Pattern: ".+", Value: "hello wide world"},
		)
	})
	defer server.Close()

	output := captureCommandStdout(t, func() error { return cmdEnv([]string{"list", "--target", "production"}) })
	for _, want := range []string{"SLUG", "DEFAULT", "PATTERN", "VALUE", "API_TOKEN", "Private token", "GREETING", "hello wide world", "Greeting text"} {
		if !strings.Contains(output, want) {
			t.Errorf("env list output missing %q:\n%s", want, output)
		}
	}
}

func TestPrintEnvVarsSanitizesControlCharacters(t *testing.T) {
	output := captureCommandStdout(t, func() error {
		return printEnvVars([]*airlockv1.EnvVarInfo{{
			Slug: "GREETING", Description: "Greeting\ttext\nline two\x01", Value: "hello\twide\nworld",
		}})
	})
	for _, want := range []string{`hello\twide\nworld`, `Greeting\ttext\nline two\x01`} {
		if !strings.Contains(output, want) {
			t.Errorf("env list output missing escaped value %q:\n%s", want, output)
		}
	}
	if strings.ContainsRune(output, '\x01') || strings.Count(output, "\n") != 2 {
		t.Errorf("env list output contains an unescaped control character:\n%s", output)
	}
}

func TestPrintEnvVarsRedactsSecretPayload(t *testing.T) {
	output := captureCommandStdout(t, func() error {
		return printEnvVars([]*airlockv1.EnvVarInfo{{
			Slug: "API_TOKEN", IsSecret: true, Configured: true,
			DefaultValue: "leaked default", Value: "leaked value",
		}})
	})
	for _, forbidden := range []string{"leaked default", "leaked value"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("env list output contains secret payload %q:\n%s", forbidden, output)
		}
	}
}

func TestCmdEnvSecretMutationsDoNotPrintValues(t *testing.T) {
	var mutations atomic.Int32
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "API_KEY", IsSecret: true, Required: true})
			return
		}
		mutations.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()
	for _, args := range [][]string{{"set", "API_KEY", "do-not-print-this", "--target", "production"}, {"clear", "API_KEY", "--target", "production"}} {
		output := captureCommandStdout(t, func() error { return cmdEnv(args) })
		if strings.Contains(output, "do-not-print-this") || !strings.Contains(output, "to check whether a restart is needed") {
			t.Fatalf("unsafe mutation output: %s", output)
		}
	}
	if mutations.Load() != 2 {
		t.Fatal("secret set/clear did not reach the server")
	}
}

func TestCmdEnvGet(t *testing.T) {
	t.Run("prints configured non-secret value", func(t *testing.T) {
		server := setupEnvCommand(t, func(w http.ResponseWriter, _ *http.Request) {
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "GREETING", Configured: true, Value: "hello wide world"})
		})
		defer server.Close()

		output := captureCommandStdout(t, func() error { return cmdEnv([]string{"get", "GREETING", "--target", "production"}) })
		if output != "hello wide world\n" {
			t.Fatalf("env get output = %q", output)
		}
	})

	t.Run("refuses secret", func(t *testing.T) {
		server := setupEnvCommand(t, func(w http.ResponseWriter, _ *http.Request) {
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "API_TOKEN", IsSecret: true, Configured: true})
		})
		defer server.Close()

		output, err := captureCommandStdoutResult(t, func() error { return cmdEnv([]string{"get", "API_TOKEN", "--target", "production"}) })
		if err == nil || !strings.Contains(err.Error(), "is secret") {
			t.Fatalf("env get error = %v", err)
		}
		if output != "" {
			t.Fatalf("env get secret output = %q", output)
		}
	})

	t.Run("prints effective default", func(t *testing.T) {
		server := setupEnvCommand(t, func(w http.ResponseWriter, _ *http.Request) {
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "GREETING", DefaultValue: "hello"})
		})
		defer server.Close()

		output := captureCommandStdout(t, func() error { return cmdEnv([]string{"get", "GREETING", "--target", "production"}) })
		if output != "hello\n" {
			t.Fatalf("env get default output = %q", output)
		}
	})
}

func TestCmdEnvSetValueBeginningWithDashes(t *testing.T) {
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "OPTIONS"})
			return
		}
		var req airlockv1.SetEnvVarValueRequest
		raw, _ := io.ReadAll(r.Body)
		if err := protojson.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode set request: %v", err)
		}
		if req.Value != "--verbose" {
			t.Errorf("set value = %q", req.Value)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()

	output := captureCommandStdout(t, func() error {
		return cmdEnv([]string{"set", "OPTIONS", "--target", "production", "--", "--verbose"})
	})
	if !strings.Contains(output, "Saved OPTIONS. Run ") {
		t.Fatalf("env set output = %q", output)
	}
}

func TestCmdEnvSetAndClear(t *testing.T) {
	var mutations atomic.Int32
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/"+envTestAgentID+"/env-vars":
			writeEnvVarList(t, w, &airlockv1.EnvVarInfo{Slug: "GREETING", Pattern: `^hello .+$`})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/"+envTestAgentID+"/env-vars/GREETING":
			mutations.Add(1)
			var req airlockv1.SetEnvVarValueRequest
			raw, _ := io.ReadAll(r.Body)
			if err := protojson.Unmarshal(raw, &req); err != nil {
				t.Errorf("decode set request: %v", err)
			}
			if req.Value != "hello wide world" {
				t.Errorf("set value = %q", req.Value)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/agents/"+envTestAgentID+"/env-vars/GREETING":
			mutations.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	defer server.Close()

	output := captureCommandStdout(t, func() error {
		return cmdEnv([]string{"set", "GREETING", "hello wide world", "--target", "production"})
	})
	if !strings.Contains(output, "Saved GREETING. Run ") || strings.Contains(output, "hello wide world") {
		t.Fatalf("env set output = %q", output)
	}
	output = captureCommandStdout(t, func() error { return cmdEnv([]string{"clear", "GREETING", "--target", "production"}) })
	if !strings.Contains(output, "Cleared GREETING. Run ") {
		t.Fatalf("env clear output = %q", output)
	}
	if mutations.Load() != 2 {
		t.Fatalf("mutation requests = %d, want 2", mutations.Load())
	}
}

func TestCmdEnvValidatesDeclarationBeforeMutation(t *testing.T) {
	var mutations atomic.Int32
	server := setupEnvCommand(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations.Add(1)
		}
		writeEnvVarList(t, w,
			&airlockv1.EnvVarInfo{Slug: "SECRET", IsSecret: true},
			&airlockv1.EnvVarInfo{Slug: "REGION", Pattern: `^[a-z]+$`},
		)
	})
	defer server.Close()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "undeclared", args: []string{"set", "MISSING", "value"}, want: "not declared"},
		{name: "pattern mismatch", args: []string{"set", "REGION", "us west"}, want: "required pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cmdEnv(append(tt.args, "--target", "production"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("cmdEnv error = %v, want %q", err, tt.want)
			}
		})
	}
	if mutations.Load() != 0 {
		t.Fatalf("mutation requests = %d, want 0", mutations.Load())
	}
}

func TestCmdEnvRejectsCodegenToken(t *testing.T) {
	t.Setenv("AIRLOCK_API_URL", "https://airlock.example")
	t.Setenv("AIRLOCK_AGENT_ID", "agent-id")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "integration-token")
	err := cmdEnv([]string{"list"})
	if err == nil || !strings.Contains(err.Error(), "operator authentication is required") {
		t.Fatalf("cmdEnv error = %v", err)
	}
}

func setupEnvCommand(t *testing.T, envHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	t.Setenv("AIRLOCK_API_URL", "")
	t.Setenv("AIRLOCK_AGENT_ID", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := newProductionTargetServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/"+envTestAgentID {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agent":{"id":"` + envTestAgentID + `","slug":"test-agent"}}`))
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessToken {
			t.Errorf("Authorization = %q", got)
		}
		envHandler(w, r)
	}))
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		server.Close()
		t.Fatal(err)
	}
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote(defaultRemoteName, agentRemoteBinding{AirlockURL: server.URL, AgentID: envTestAgentID, Slug: "test-agent"})
	if err := writeAgentBinding(dir, binding); err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Chdir(dir)
	return server
}

func writeEnvVarList(t *testing.T, w http.ResponseWriter, envVars ...*airlockv1.EnvVarInfo) {
	t.Helper()
	encoded, err := protojson.Marshal(&airlockv1.ListEnvVarsResponse{EnvVars: envVars})
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}
