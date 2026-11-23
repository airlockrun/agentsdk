package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
)

func captureStatusGuidanceJourneys(t *testing.T) []reviewReceipt {
	t.Helper()
	var receipts []reviewReceipt
	for _, scenario := range []string{"building", "queued", "candidate only", "saved running", "saved succeeded", "saved failed", "invalid saved task", "ready", "ready stale terminal cache", "failed older runtime", "stopped", "hosted building", "hosted queued", "hosted candidate only", "hosted saved running", "wait completed", "hosted wait completed", "own wait completed", "hosted own wait completed", "own wait timeout", "hosted timeout", "failed wait", "custom timeout and directory", "inspect building"} {
		for _, machine := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", scenario, machine), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				for _, name := range []string{"AIRLOCK_DEVELOPMENT_SESSION", "AIRLOCK_INTEGRATION_TOKEN", "AIRLOCK_TOKEN", "AIRLOCK_DEVELOPER_RUN_ID"} {
					t.Setenv(name, "")
				}
				dir := filepath.Join(t.TempDir(), "app 'quoted'")
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				t.Chdir(dir)
				mustWrite(t, filepath.Join(dir, "go.mod"), "module example.test/app\n\ngo 1.26\n\ntool github.com/airlockrun/agentsdk/cmd/air\n")
				hosted := strings.HasPrefix(scenario, "hosted")
				stateScenario := strings.TrimPrefix(scenario, "hosted ")
				reads, mutations := 0, 0
				prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet && r.Method != http.MethodHead {
						mutations++
						t.Errorf("observation mutated: %s %s", r.Method, r.URL)
					}
					switch r.URL.Path {
					case "/api/v1/agents/" + testBuildAgentID:
						fmt.Fprintf(w, `{"agent":{"id":%q,"name":"MaxFrame","slug":"maxframe"}}`, testBuildAgentID)
					case prefix + previewRuntimeID, prefix + previewName:
						reads++
						status, task, taskStatus, candidate, running := "active", previewTaskID, "running", previewWorkspaceID, true
						switch stateScenario {
						case "queued":
							taskStatus = "queued"
						case "candidate only":
							task, taskStatus = "", ""
						case "saved running", "saved succeeded", "saved failed", "invalid saved task":
							task, taskStatus, candidate = "", "", ""
						case "ready":
							taskStatus, candidate = "succeeded", ""
						case "ready stale terminal cache":
							taskStatus, candidate = "succeeded", ""
						case "failed older runtime", "failed wait":
							taskStatus, candidate = "failed", ""
						case "stopped":
							status, taskStatus, candidate, running = "stopped", "succeeded", "", false
						case "wait completed", "own wait completed":
							if reads > 1 {
								taskStatus, candidate = "succeeded", ""
							}
						}
						fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"personal_preview","name":%q,"ownerUserId":%q,"ownerWorkspaceId":%q,"status":%q,"running":%t,"imageRef":"older-serving-image","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":%q,"pendingCandidateRevisionId":%q,"routeBaseUrl":"https://maxframe-preview.example.test"}}`, previewRuntimeID, testBuildAgentID, previewRuntimeID, previewName, previewOwnerID, previewWorkspaceID, status, running, testSourceRevision, task, taskStatus, candidate)
					case "/api/v1/tasks/" + previewTaskID:
						status := "running"
						if stateScenario == "saved succeeded" || stateScenario == "own wait completed" {
							status = "succeeded"
						}
						if stateScenario == "own wait completed" {
							w.Header().Set("X-Airlock-Build-ID", testBuildID)
						}
						if scenario == "saved failed" {
							status = "failed"
						}
						fmt.Fprintf(w, `{"id":%q,"kind":"upgrade","status":%q}`, previewTaskID, status)
					case prefix + previewName + "/builds/" + testBuildID:
						fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"complete"}}`, testBuildID, previewRuntimeID)
					case prefix + previewName + "/source":
						if r.Method != http.MethodHead {
							t.Errorf("unexpected source request: %s", r.Method)
						}
						w.Header().Set(wire.SourceStateHeader, "sha256:canonical-source")
						w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
						w.Header().Set(wire.SourceGenerationHeader, "2")
						w.Header().Set("ETag", (wire.SourceVersion{Revision: testSourceRevision, Generation: 2}).ETag())
					default:
						t.Errorf("unexpected request: %s", r.URL)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				if err := saveLoginCredentials(server.URL, "review@example.test", testAccessToken, ""); err != nil {
					t.Fatal(err)
				}
				state := targetSourceBinding{Name: previewName, OwnerUserID: previewOwnerID}
				if strings.Contains(scenario, "saved") || scenario == "ready stale terminal cache" || scenario == "own wait timeout" || stateScenario == "own wait completed" {
					state.PendingTask, state.PendingSourceState = previewTaskID, "sha256:accepted-source"
				}
				if scenario == "invalid saved task" {
					state.PendingTask = "invalid-cache-id"
				}
				binding := agentBinding{}
				binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Slug: "maxframe", Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}, Targets: map[string]targetSourceBinding{previewRuntimeID: state}})
				if err := writeAgentBinding(dir, binding); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(dir, agentBindingPath))
				if err != nil {
					t.Fatal(err)
				}
				if hosted {
					for name, value := range map[string]string{"AIRLOCK_DEVELOPMENT_SESSION": previewWorkspaceID, "AIRLOCK_DEVELOPER_SESSION_ID": previewWorkspaceID, "AIRLOCK_APP_ID": testBuildAgentID, "AIRLOCK_AGENT_ID": previewRuntimeID, "AIRLOCK_DEPLOYMENT_TARGET": previewName, "AIRLOCK_API_URL": server.URL, "AIRLOCK_TOKEN": testAccessToken} {
						t.Setenv(name, value)
					}
				}
				args := []string{"status", "--remote", "corp", "--target", "preview"}
				if scenario == "inspect building" {
					args[0] = "inspect"
				}
				if stateScenario == "wait completed" || stateScenario == "own wait completed" || scenario == "failed wait" {
					args = append(args, "--wait", "--timeout", "1s")
				}
				if scenario == "own wait timeout" || scenario == "hosted timeout" {
					args = append(args, "--wait", "--timeout", "20ms")
				}
				if scenario == "custom timeout and directory" {
					t.Chdir(filepath.Dir(dir))
					args = append(args, "--dir", dir, "--timeout", "3m")
				}
				if machine {
					args = append(args, "--json")
				}
				stdout, err := os.CreateTemp(t.TempDir(), "stdout")
				if err != nil {
					t.Fatal(err)
				}
				defer stdout.Close()
				stderr, err := os.CreateTemp(t.TempDir(), "stderr")
				if err != nil {
					t.Fatal(err)
				}
				defer stderr.Close()
				oldOut, oldErr := os.Stdout, os.Stderr
				var commandErr error
				func() {
					os.Stdout, os.Stderr = stdout, stderr
					defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
					commandErr = runReviewCLI(t, args, stdout, stderr)
				}()
				out, err := os.ReadFile(stdout.Name())
				if err != nil {
					t.Fatal(err)
				}
				diagnostics, err := os.ReadFile(stderr.Name())
				if err != nil {
					t.Fatal(err)
				}
				after, err := os.ReadFile(filepath.Join(dir, agentBindingPath))
				if err != nil {
					t.Fatal(err)
				}
				if (stateScenario != "own wait completed" && string(before) != string(after)) || mutations != 0 {
					t.Fatal("status guidance mutated source state")
				}
				if stateScenario == "own wait completed" {
					updated, _, err := loadAgentBinding(dir)
					if err != nil {
						t.Fatal(err)
					}
					remote, _ := updated.remote("corp")
					if remote.Targets[previewRuntimeID].PendingTask != "" || remote.Targets[previewRuntimeID].SourceState != "sha256:canonical-source" {
						t.Fatal("own accepted task was not reconciled")
					}
				}
				receipt := reviewReceipt{Journey: scenario, Args: args, ExitCode: reviewExitCode(commandErr), Stdout: string(out), Stderr: string(diagnostics), Mutations: mutations}
				receipts = append(receipts, receipt)
				raw, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("RECEIPT %s", raw)
				if machine {
					decodeResult(t, receipt.Stdout)
				}
				wantError := scenario == "own wait timeout" || scenario == "hosted timeout" || scenario == "failed wait"
				if (commandErr != nil) != wantError {
					t.Fatalf("unexpected result: %v %+v", commandErr, receipt)
				}
			})
		}
	}
	return receipts
}

func TestStatusGuidanceReviewCapture(t *testing.T) {
	receipts := captureStatusGuidanceJourneys(t)
	if path := os.Getenv("AIR_CLI_STATUS_RECEIPTS"); path != "" {
		raw, err := json.MarshalIndent(receipts, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusGuidanceProgressAndTerminalStates(t *testing.T) {
	receipts := captureStatusGuidanceJourneys(t)
	for _, receipt := range receipts {
		scenario, machine := receipt.Journey, jsonRequested(receipt.Args)
		stateScenario := strings.TrimPrefix(scenario, "hosted ")
		wantWait := stateScenario == "building" || stateScenario == "queued" || stateScenario == "candidate only" || stateScenario == "saved running" || stateScenario == "own wait timeout" || scenario == "custom timeout and directory" || scenario == "hosted timeout"
		if machine {
			result := decodeResult(t, receipt.Stdout)
			wait := false
			for _, action := range result.Guidance {
				wait = wait || containsArgument(action.Args, "--wait")
				if containsArgument(action.Args, "--wait") && (!strings.Contains(shellCommand(action), "--target "+previewName) || !strings.Contains(shellCommand(action), "--agent "+testBuildAgentID)) {
					t.Fatalf("unpinned machine wait: %+v", action)
				}
			}
			if wait != wantWait {
				t.Fatalf("wrong wait guidance: %+v", receipt)
			}
			if strings.Contains(receipt.Stdout, `"when"`) || strings.Contains(receipt.Stdout, "Yield for HOST") {
				t.Fatalf("context-dependent guidance: %+v", receipt)
			}
			if scenario == "own wait timeout" && (result.Operation == nil || result.Operation.Admission != "accepted" || result.Operation.TaskID != previewTaskID || result.Error.Code != "observation_timeout" || !strings.Contains(receipt.Stdout, "older-serving-image")) {
				t.Fatalf("timeout lost accepted task/snapshot: %+v", receipt)
			}
			if scenario == "failed older runtime" || scenario == "failed wait" {
				if !strings.Contains(receipt.Stdout, `"deploy","status"`) || !strings.Contains(receipt.Stdout, `"--logs"`) {
					t.Fatalf("missing diagnostics: %+v", receipt)
				}
			}
			continue
		}
		hasWait := strings.Contains(receipt.Stdout, "--wait")
		if hasWait != wantWait {
			t.Fatalf("wrong human wait: %+v", receipt)
		}
		if wantWait && (!strings.Contains(receipt.Stdout, "--remote corp --target preview --wait") || strings.Contains(receipt.Stdout, testBuildAgentID) || strings.Contains(receipt.Stdout, previewName)) {
			t.Fatalf("unfriendly wait: %+v", receipt)
		}
		if scenario == "custom timeout and directory" && (!strings.Contains(receipt.Stdout, "go -C '") || !strings.Contains(receipt.Stdout, "--timeout 3m0s")) {
			t.Fatalf("lost directory/timeout: %+v", receipt)
		}
		if (scenario == "building" || scenario == "queued") && !strings.Contains(receipt.Stdout, "current version is still running") {
			t.Fatalf("lost serving state: %+v", receipt)
		}
	}
	for _, hosted := range receipts {
		if !strings.HasPrefix(hosted.Journey, "hosted ") || hosted.Journey == "hosted timeout" {
			continue
		}
		for _, external := range receipts {
			if external.Journey != strings.TrimPrefix(hosted.Journey, "hosted ") || jsonRequested(external.Args) != jsonRequested(hosted.Args) {
				continue
			}
			if jsonRequested(hosted.Args) {
				h, e := decodeResult(t, hosted.Stdout), decodeResult(t, external.Stdout)
				if !reflect.DeepEqual(h.Guidance, e.Guidance) || !reflect.DeepEqual(h.Data, e.Data) || h.Status != e.Status {
					t.Fatalf("hosted rendering differs from external state: %s\n%s", hosted.Stdout, external.Stdout)
				}
			} else {
				normalize := func(s string) string {
					return strings.ReplaceAll(s, "Application · Preview · corp", "MaxFrame · Preview · corp")
				}
				if normalize(hosted.Stdout) != normalize(external.Stdout) {
					t.Fatalf("hosted human body differs: %s\n%s", hosted.Stdout, external.Stdout)
				}
			}
		}
	}
}

func TestTargetWaitRequiresActiveRunningImage(t *testing.T) {
	for _, tc := range []struct {
		name, status, image string
		running             bool
		ready               bool
	}{
		{"ready active", "active", "verified-image", true, true},
		{"wrong running status", "running", "verified-image", true, false},
		{"missing image", "active", "", true, false},
		{"not running", "active", "verified-image", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("wait mutated target")
				}
				fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"production","name":"production","status":%q,"running":%t,"imageRef":%q}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, tc.status, tc.running, tc.image)
			}))
			defer server.Close()
			s := selectedDeployment{flags: targetFlags{target: "production"}, baseURL: server.URL, app: agentRemoteBinding{AgentID: testBuildAgentID}, info: &airlockv1.DeploymentInfo{Id: testBuildAgentID}}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			err := s.waitRunning(ctx, time.Millisecond, readinessRuntime)
			if tc.ready && err != nil || !tc.ready && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("readiness=%t: %v", tc.ready, err)
			}
		})
	}
}

func TestLifecycleWaitObservesRuntimeNotHistoricalBuildFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	dir := t.TempDir()
	posts := 0
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("lifecycle lost ordinary authorization")
		}
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe"}}`, testBuildAgentID)
		case prefix:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production","status":"active","running":true,"imageRef":"serving-version","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":"failed"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, testSourceRevision, previewTaskID)
		case prefix + "/restart":
			if r.Method != http.MethodPost {
				t.Error("restart did not use its exact operation")
			}
			posts++
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected lifecycle/task/replay route: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.test", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	binding := agentBinding{}
	binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	output, err := invokeFixture(t, "restart", "--dir", dir, "--wait", "--json")
	result := decodeResult(t, output)
	if err != nil || result.Status != "completed" || result.Error != nil || result.Operation.Action != "restart" || posts != 1 || result.Data.(map[string]any)["deployment"].(map[string]any)["buildTaskStatus"] != "failed" {
		t.Fatalf("lifecycle conflated deployment outcome or replayed: %v posts=%d %s", err, posts, output)
	}
	t.Logf("JSON stdout:\n%s", output)
}

func TestRestartPreservesTargetAndApproval(t *testing.T) {
	for _, tc := range []struct {
		target string
		denied bool
	}{
		{previewName, false}, {previewName, true},
		{"production", false}, {"production", true},
	} {
		t.Run(fmt.Sprintf("%s/approval=%t", tc.target, tc.denied), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
			t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
			t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
			t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
			t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
			t.Setenv("AIRLOCK_TOKEN", testAccessToken)
			t.Setenv("SOL_COMMAND_CONTROL_SOCKET", "")
			posts, reads := 0, 0
			base := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + tc.target
			runtimeID, kind := previewRuntimeID, "personal_preview"
			if tc.target == "production" {
				runtimeID, kind = testBuildAgentID, "production"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("restart lost authentication")
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == base+"/restart":
					posts++
					if tc.denied {
						w.Header().Set("Airlock-Operation-Challenge", previewTaskID)
						w.WriteHeader(http.StatusPreconditionRequired)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && r.URL.Path == base:
					reads++
					fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":%q,"name":%q,"ownerWorkspaceId":%q,"status":"active","running":true,"imageRef":"test-image"}}`, runtimeID, runtimeID, testBuildAgentID, kind, tc.target, previewWorkspaceID)
				default:
					t.Errorf("unexpected restart request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			t.Setenv("AIRLOCK_API_URL", server.URL)
			var result error
			captureCommandStdout(t, func() error {
				result = run([]string{"restart", "--dir", t.TempDir(), "--target", tc.target, "--wait", "--timeout", time.Second.String()})
				return nil
			})
			if tc.denied {
				var approval *approvalRequiredError
				if !errors.As(result, &approval) || approval.id != previewTaskID || reads != 1 {
					t.Fatalf("approval result=%v reads=%d", result, reads)
				}
			} else if result != nil || reads != 2 {
				t.Fatalf("restart result=%v reads=%d", result, reads)
			}
			if posts != 1 {
				t.Fatalf("restart submitted %d times", posts)
			}
		})
	}
}

func TestTargetStatusJSONIncludesEnvironmentRevisionsOnly(t *testing.T) {
	applied := int64(7)
	s := selectedDeployment{flags: targetFlags{json: true}, remoteName: "origin", info: &airlockv1.DeploymentInfo{Name: "production", DesiredEnvRevision: 8, AppliedEnvRevision: &applied, EnvConfigurationState: "restart_required", RestartRequired: true}}
	output := captureCommandStdout(t, s.print)
	var result struct {
		Deployment struct {
			Desired string `json:"desiredEnvRevision"`
			Applied string `json:"appliedEnvRevision"`
			State   string `json:"envConfigurationState"`
			Restart bool   `json:"restartRequired"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Deployment.Desired != "8" || result.Deployment.Applied != "7" || result.Deployment.State != "restart_required" || !result.Deployment.Restart {
		t.Fatalf("missing environment status: %s", output)
	}
	if strings.Contains(output, "envStartAttempt") || strings.Contains(output, "values") {
		t.Fatal("status exposes private startup data")
	}
}

func TestHostedRestartUsesOneExplicitLifecycleRequest(t *testing.T) {
	for _, status := range []int{204, 403, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
			t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
			t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
			t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
			t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
			t.Setenv("AIRLOCK_TOKEN", testAccessToken)
			posts := 0
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("lost execution credential")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET " + prefix:
					fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"production","name":"production","running":true}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID)
				case "POST " + prefix + "/restart":
					posts++
					w.WriteHeader(status)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			t.Setenv("AIRLOCK_API_URL", server.URL)
			err := run([]string{"restart", "--target", "production", "--dir", t.TempDir()})
			if status == 204 && err != nil {
				t.Fatal(err)
			}
			if status != 204 && !hasHTTPStatus(err, status) {
				t.Fatalf("status not preserved: %v", err)
			}
			if posts != 1 {
				t.Fatalf("restart issued %d lifecycle requests", posts)
			}
		})
	}
}

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

func TestTargetAppOutput(t *testing.T) {
	for _, workspaceURL := range []string{"https://dashboard.example.test/chat?app=" + testBuildAgentID, "https://dashboard.example.test/chat/" + previewWorkspaceID, ""} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", workspaceURL, asJSON), func(t *testing.T) {
				s := selectedDeployment{flags: targetFlags{json: asJSON}, token: "secret-not-for-output", info: &airlockv1.DeploymentInfo{Id: previewRuntimeID, Name: previewName, WorkspaceUrl: workspaceURL, RouteBaseUrl: "https://runtime.example.test"}}
				out := captureCommandStdout(t, s.print)
				if strings.Contains(out, s.token) {
					t.Fatal("credential printed")
				}
				if asJSON {
					var result struct {
						Deployment struct {
							WorkspaceURL string `json:"workspaceUrl"`
							RouteBaseURL string `json:"routeBaseUrl"`
						} `json:"deployment"`
					}
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatal(err)
					}
					if result.Deployment.WorkspaceURL != workspaceURL || result.Deployment.RouteBaseURL != s.info.RouteBaseUrl {
						t.Fatalf("JSON: %s", out)
					}
				} else {
					if !strings.Contains(out, "App URL: "+s.info.RouteBaseUrl) || strings.Contains(out, "Workspace URL:") {
						t.Fatalf("text: %s", out)
					}
					if workspaceURL != "" && strings.Contains(out, workspaceURL) {
						t.Fatalf("workspace URL printed instead of app URL: %s", out)
					}
				}
			})
		}
	}
}

func TestTargetAppURLUsesSelectedTarget(t *testing.T) {
	for _, tt := range []struct{ name, kind, appURL string }{
		{"production", "production", "https://server-production.example.test"},
		{previewName, "personal_preview", "https://server-exact-preview.example.test"},
		{"unavailable", "personal_preview", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := selectedDeployment{info: &airlockv1.DeploymentInfo{
				Name: tt.name, Kind: tt.kind, RouteBaseUrl: tt.appURL,
				WorkspaceUrl: "https://dashboard.example.test/chat/" + previewWorkspaceID,
			}}
			out := captureCommandStdout(t, func() error { s.printAppURL(); return nil })
			want := "App URL: " + tt.appURL + "\n"
			if tt.appURL == "" {
				want = "App URL: unavailable (server returned no route URL for this target)\n"
			}
			if out != want {
				t.Fatalf("output=%q want=%q", out, want)
			}
		})
	}
}

func TestHostedStatusUsesServerAppURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
	t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
	t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
	t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
	t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
	t.Setenv("AIRLOCK_TOKEN", testAccessToken)
	workspaceURL := "https://public.example.test/prefix%20name/chat/22222222-2222-4222-8222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testAccessToken || r.URL.Path != "/api/v1/agents/"+testBuildAgentID+"/deployments/"+previewName {
			t.Errorf("incorrect authenticated target: %s %s", r.Method, r.URL)
		}
		fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q,"workspaceUrl":%q,"routeBaseUrl":"https://runtime.example.test"}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID, workspaceURL)
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	for _, args := range [][]string{{"--dir", t.TempDir()}, {"--dir", t.TempDir(), "--json"}} {
		out := captureCommandStdout(t, func() error { return cmdTargetStatus(args) })
		if !strings.Contains(out, "https://runtime.example.test") || strings.Contains(out, testAccessToken) {
			t.Fatalf("status: %s", out)
		}
	}
}
