package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/sourcebundle"
	"github.com/airlockrun/agentsdk/wire"
)

type reviewReceipt struct {
	Journey   string   `json:"journey"`
	Args      []string `json:"args"`
	ExitCode  int      `json:"exitCode"`
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Mutations int      `json:"mutations"`
}

func runReviewCLI(t *testing.T, args []string, stdout, stderr *os.File) error {
	t.Helper()
	if binary := os.Getenv("AIR_CLI_CAPTURE_BINARY"); binary != "" {
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Stdout, command.Stderr = stdout, stderr
		return command.Run()
	}
	err := invokeCLI(args, stdout)
	if err != nil && !jsonRequested(args) {
		fmt.Fprintf(stderr, "air: %v\n", err)
	}
	return err
}

func reviewExitCode(err error) int {
	var child *exec.ExitError
	if errors.As(err, &child) {
		return child.ExitCode()
	}
	return exitCode(err)
}

func captureReviewCommand(t *testing.T, journey string, args []string) reviewReceipt {
	t.Helper()
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
	return reviewReceipt{Journey: journey, Args: args, ExitCode: reviewExitCode(commandErr), Stdout: string(out), Stderr: string(diagnostics)}
}

// The same fixture runs against each frozen source version. Receipts contain
// actual boundary output, including stderr and process-equivalent exit codes.
func captureCLIReviewJourneys(t *testing.T) []reviewReceipt {
	var receipts []reviewReceipt
	for _, journey := range []string{"preview external", "preview hosted", "preview wait external", "preview wait hosted", "building", "ready", "failed older runtime", "restart needed", "env set", "tool completed", "agent pending", "help no auth", "invalid args", "accepted preview observation 403", "accepted preview observation 401", "accepted POST observation 403", "denied POST 403", "tool preflight 403", "denied tool POST 403", "accepted host observation 403", "accepted host observation 401"} {
		for _, machine := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", journey, machine), func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				for _, key := range []string{"AIRLOCK_DEVELOPMENT_SESSION", "AIRLOCK_INTEGRATION_TOKEN", "AIRLOCK_DEVELOPER_RUN_ID", "AIRLOCK_WORKSPACE_COMMAND_TASK", "AIRLOCK_WORKSPACE_COMMAND_OWNER", "SOL_COMMAND_CONTROL_SOCKET"} {
					t.Setenv(key, "")
				}
				dir := t.TempDir()
				t.Chdir(dir)
				mustWrite(t, filepath.Join(dir, "go.mod"), "module example.test/app\n\ngo 1.26\n\ntool github.com/airlockrun/agentsdk/cmd/air\n")
				mutations := 0
				admitted := false
				waitingPreview := strings.HasPrefix(journey, "preview wait")
				archiveState := ""
				runtimeReads := 0
				prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" && r.Method != "HEAD" {
						mutations++
					}
					switch r.URL.Path {
					case "/api/v1/agents/" + testBuildAgentID:
						fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe","name":"MaxFrame"}}`, testBuildAgentID)
					case "/api/v1/agent-sdk":
						fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
					case "/api/v1/hosts/" + fixtureHostID + "/shell":
						fmt.Fprintf(w, `{"job":{"id":%q,"requestId":%q,"hostId":%q,"kind":"shell","status":"queued"}}`, fixtureJobID, fixtureJobID, fixtureHostID)
					case "/api/v1/host-management-jobs/" + fixtureJobID:
						code := 403
						if strings.HasSuffix(journey, "401") {
							code = 401
						}
						http.Error(w, `{"error":"fixture host observation denied"}`, code)
					case prefix + "production", prefix + "preview", prefix + previewName, prefix + previewRuntimeID:
						runtimeReads++
						if journey == "tool preflight 403" && runtimeReads > 1 {
							http.Error(w, `{"error":"fixture preflight denied"}`, 403)
							return
						}
						if journey == "accepted POST observation 403" && admitted {
							http.Error(w, `{"error":"fixture observation denied"}`, 403)
							return
						}
						id, kind, name := testBuildAgentID, "production", "production"
						if !strings.HasSuffix(r.URL.Path, "production") {
							id, kind, name = previewRuntimeID, "personal_preview", previewName
						}
						taskStatus, candidate := "", ""
						if journey == "building" {
							taskStatus, candidate = "running", previewWorkspaceID
						}
						if journey == "failed older runtime" {
							taskStatus = "failed"
						}
						configuration := `"envConfigurationState":"current","restartRequired":false`
						if journey == "restart needed" {
							configuration = `"envConfigurationState":"restart_required","restartRequired":true`
						}
						fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":%q,"name":%q,"ownerUserId":%q,"ownerWorkspaceId":%q,"status":"active","running":true,"imageRef":"serving-image","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":%q,"pendingCandidateRevisionId":%q,"routeBaseUrl":"https://maxframe.example.test",%s}}`, id, testBuildAgentID, id, kind, name, previewOwnerID, previewWorkspaceID, testSourceRevision, previewTaskID, taskStatus, candidate, configuration)
					case prefix + previewName + "/source":
						if r.Method == http.MethodHead {
							w.Header().Set(wire.SourceStateHeader, archiveState)
							w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
							w.Header().Set(wire.SourceGenerationHeader, "2")
							w.Header().Set("ETag", (wire.SourceVersion{Revision: testSourceRevision, Generation: 2}).ETag())
							return
						}
						state, err := sourcebundle.ExtractArchiveState(r.Body, t.TempDir())
						if err != nil {
							t.Error(err)
						}
						admitted = true
						archiveState = state
						w.Header().Set(wire.SourceStateHeader, state)
						w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
						w.Header().Set(wire.SourceGenerationHeader, "2")
						w.Header().Set(wire.SourceTaskHeader, previewTaskID)
						w.WriteHeader(http.StatusAccepted)
					case "/api/v1/tasks/" + previewTaskID:
						if waitingPreview {
							w.Header().Set("X-Airlock-Build-ID", testBuildID)
							fmt.Fprintf(w, `{"id":%q,"kind":"upgrade","status":"succeeded"}`, previewTaskID)
							return
						}
						code := 403
						if strings.HasSuffix(journey, "401") {
							code = 401
						}
						http.Error(w, `{"error":"fixture observation denied"}`, code)
					case prefix + previewName + "/builds/" + testBuildID:
						fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"complete"}}`, testBuildID, previewRuntimeID)
					case prefix + "production/restart":
						if journey == "denied POST 403" {
							http.Error(w, `{"error":"fixture admission denied"}`, 403)
							return
						}
						admitted = true
						fmt.Fprint(w, `{}`)
					case prefix + "production/env-vars":
						fmt.Fprint(w, `{"envVars":[{"slug":"REGION","configured":true,"value":"eu","required":true}]}`)
					case prefix + "production/env-vars/REGION":
						fmt.Fprint(w, `{}`)
					case prefix + "production/tools/report/invoke":
						if journey == "denied tool POST 403" {
							http.Error(w, `{"error":"fixture tool admission denied"}`, 403)
							return
						}
						fmt.Fprintf(w, `{"runId":%q,"resultJson":"{\"title\":\"Report\",\"appOwnedId\":\"%s\"}"}`, previewTaskID, testSourceRevision)
					case prefix + "production/agents/research/runs/" + previewTaskID:
						fmt.Fprintf(w, `{"run":{"id":%q,"definition":"research","status":"running"}}`, previewTaskID)
					default:
						t.Errorf("unexpected review request: %s %s", r.Method, r.URL)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				if journey != "help no auth" && journey != "invalid args" {
					if err := saveLoginCredentials(server.URL, "review@example.test", testAccessToken, ""); err != nil {
						t.Fatal(err)
					}
				}
				binding := agentBinding{}
				binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Slug: "maxframe", Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
				if err := writeAgentBinding(dir, binding); err != nil {
					t.Fatal(err)
				}
				args := []string{"status"}
				switch journey {
				case "preview external", "preview hosted", "preview wait external", "preview wait hosted", "accepted preview observation 403", "accepted preview observation 401":
					args = []string{"preview", "-m", "Review candidate"}
					if strings.HasPrefix(journey, "preview") {
						args = append(args, "--remote", "corp")
					}
					if waitingPreview {
						args = append(args, "--launch", "--wait", "--timeout", "1s")
					}
					if strings.HasPrefix(journey, "accepted") {
						args = append(args, "--wait", "--timeout", "1s")
					}
				case "env set":
					args = []string{"env", "set", "REGION", "us"}
				case "tool completed", "tool preflight 403", "denied tool POST 403":
					args = []string{"tools", "invoke", "report", "--input", `{}`}
				case "agent pending":
					args = []string{"agent", "get", "research", previewTaskID}
				case "help no auth":
					args = []string{"preview", "--help"}
				case "invalid args":
					args = []string{"status", "--unsupported-flag"}
				case "accepted POST observation 403", "denied POST 403":
					args = []string{"restart"}
				case "accepted host observation 403", "accepted host observation 401":
					args = []string{"hosts", "exec", fixtureHostID, "--url", server.URL, "--request-id", fixtureJobID}
				}
				if journey == "preview hosted" || journey == "preview wait hosted" {
					for key, value := range map[string]string{"AIRLOCK_DEVELOPMENT_SESSION": previewWorkspaceID, "AIRLOCK_DEVELOPER_SESSION_ID": previewWorkspaceID, "AIRLOCK_DEVELOPER_RUN_ID": previewTaskID, "AIRLOCK_APP_ID": testBuildAgentID, "AIRLOCK_AGENT_ID": previewRuntimeID, "AIRLOCK_DEPLOYMENT_TARGET": previewName, "AIRLOCK_API_URL": server.URL, "AIRLOCK_TOKEN": testAccessToken, "AIRLOCK_DEPLOYMENT_SOURCE_ETAG": ""} {
						t.Setenv(key, value)
					}
				}
				if machine {
					args = append(args, "--json")
				}
				if strings.Contains(journey, "host observation") {
					args = append(args, "--", "fixture-not-executed")
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
				receipt := reviewReceipt{Journey: journey, Args: args, ExitCode: reviewExitCode(commandErr), Stdout: string(out), Stderr: string(diagnostics), Mutations: mutations}
				receipts = append(receipts, receipt)
				raw, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("RECEIPT %s", raw)
				if machine {
					decodeResult(t, string(out))
				}
				if strings.HasPrefix(journey, "accepted") || journey == "denied POST 403" {
					if commandErr == nil || mutations != 1 {
						t.Fatalf("expected one mutation then error: %+v", receipt)
					}
				} else if commandErr != nil && journey != "invalid args" && journey != "tool preflight 403" && journey != "denied tool POST 403" {
					t.Fatalf("unexpected journey error: %v", commandErr)
				}
			})
		}
	}
	if path := os.Getenv("AIR_CLI_REVIEW_RECEIPTS"); path != "" {
		raw, err := json.MarshalIndent(receipts, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return receipts
}

func TestCLIReviewJourneys(t *testing.T) { captureCLIReviewJourneys(t) }

func TestCLIReviewAdmissionAndHumanFacts(t *testing.T) {
	receipts := captureCLIReviewJourneys(t)
	for _, receipt := range receipts {
		machine := jsonRequested(receipt.Args)
		if machine {
			result := decodeResult(t, receipt.Stdout)
			if strings.HasPrefix(receipt.Journey, "accepted") {
				if result.Operation == nil || result.Operation.Admission != "accepted" || !strings.HasPrefix(result.Error.Code, "observation_") {
					t.Fatalf("lost admitted effect: %+v", receipt)
				}
				if strings.Contains(receipt.Journey, "preview") && result.Operation.TaskID != previewTaskID {
					t.Fatalf("lost task: %+v", receipt)
				}
				if strings.Contains(receipt.Journey, "host observation") && (result.Operation.JobID != fixtureJobID || result.Operation.RequestID != fixtureJobID) {
					t.Fatalf("lost accepted job: %+v", receipt)
				}
			}
			if strings.HasPrefix(receipt.Journey, "denied") && (result.Operation == nil || result.Operation.Admission != "blocked") {
				t.Fatalf("admission denial lost: %+v", receipt)
			}
			if receipt.Journey == "tool preflight 403" && (result.Operation.Admission != "unknown" || receipt.Mutations != 0) {
				t.Fatalf("read denial changed admission: %+v", receipt)
			}
			continue
		}
		switch receipt.Journey {
		case "preview external":
			if strings.Contains(receipt.Stdout, testBuildAgentID) || strings.Contains(receipt.Stdout, previewName) || !strings.Contains(receipt.Stdout, "--target preview --wait") || strings.Contains(receipt.Stdout, "--timeout") {
				t.Fatalf("unfriendly hint: %+v", receipt)
			}
		case "ready":
			if strings.Contains(receipt.Stdout, "configuration") || strings.Contains(receipt.Stdout, "Restart") {
				t.Fatalf("nonactionable configuration: %+v", receipt)
			}
		case "restart needed":
			if !strings.Contains(receipt.Stdout, "Restart needed") {
				t.Fatalf("actionable configuration hidden: %+v", receipt)
			}
		case "tool completed":
			if strings.Contains(receipt.Stdout, "Run:") || !strings.Contains(receipt.Stdout, testSourceRevision) {
				t.Fatalf("tool metadata leaked or payload stripped: %+v", receipt)
			}
		case "agent pending":
			if !strings.Contains(receipt.Stdout, "Run: "+previewTaskID) {
				t.Fatalf("pending handle hidden: %+v", receipt)
			}
		}
	}
	for _, hosted := range receipts {
		if hosted.Journey != "preview hosted" && hosted.Journey != "preview wait hosted" {
			continue
		}
		for _, external := range receipts {
			if external.Journey != strings.Replace(hosted.Journey, "hosted", "external", 1) || jsonRequested(external.Args) != jsonRequested(hosted.Args) {
				continue
			}
			if jsonRequested(hosted.Args) {
				h, e := decodeResult(t, hosted.Stdout), decodeResult(t, external.Stdout)
				if !reflect.DeepEqual(h.Guidance, e.Guidance) || !reflect.DeepEqual(h.Data, e.Data) || h.Status != e.Status || !reflect.DeepEqual(h.Operation, e.Operation) {
					t.Fatalf("preview context altered result/guidance: %s\n%s", hosted.Stdout, external.Stdout)
				}
				if strings.Contains(hosted.Journey, "wait") && (h.Status != "completed" || h.Operation.Admission != "accepted" || h.Operation.TaskID != previewTaskID || !strings.Contains(hosted.Stdout, `"status":"active"`) || !strings.Contains(hosted.Stdout, `"imageRef":"serving-image"`)) {
					t.Fatalf("built-in preview wait did not converge: %s", hosted.Stdout)
				}
			} else {
				normalize := func(s string) string {
					return strings.ReplaceAll(s, "Application · Preview · corp", "MaxFrame · Preview · corp")
				}
				if normalize(hosted.Stdout) != normalize(external.Stdout) {
					t.Fatalf("preview human rendering differs: %s\n%s", hosted.Stdout, external.Stdout)
				}
			}
		}
	}
}
