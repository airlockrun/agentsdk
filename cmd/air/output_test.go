package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/sourcebundle"
	"github.com/airlockrun/agentsdk/wire"
)

func invokeFixture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return captureCommandStdoutResult(t, func() error { return invokeCLI(args, os.Stdout) })
}

func decodeResult(t *testing.T, output string) cliResult {
	t.Helper()
	var result cliResult
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("invalid result: %v\n%s", err, output)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("not exactly one document: %v\n%s", err, output)
	}
	if result.SchemaVersion != 1 || result.Command == "" || result.Status == "" || result.Data == nil {
		t.Fatalf("incomplete result: %s", output)
	}
	return result
}

func TestCLIHelpAllPublicCommandsIsLocal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_TOKEN", "must-not-appear")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	for _, command := range publicCommands {
		t.Run(command.Command, func(t *testing.T) {
			for _, suffix := range []string{"--help", "-h"} {
				args := append(strings.Fields(command.Command), suffix)
				output, err := invokeFixture(t, args...)
				if err != nil || !strings.Contains(output, "Usage:") || strings.Contains(output, "must-not-appear") {
					t.Fatalf("%v: %v %s", args, err, output)
				}
			}
			args := append([]string{"help"}, strings.Fields(command.Command)...)
			args = append(args, "--json")
			output, err := invokeFixture(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			result := decodeResult(t, output)
			if result.Command != "help" || result.Status != "completed" {
				t.Fatalf("help result: %s", output)
			}
		})
	}
	output, err := invokeFixture(t, "help")
	if err != nil || strings.Count(output, "\n") > 16 || strings.Contains(output, "context delete") || strings.Contains(output, "supervisor-protocol") {
		t.Fatalf("root discovery: %v %s", err, output)
	}
}

func TestCLIJSONErrorsAllPublicCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	for _, command := range publicCommands {
		if command.Command == "version" {
			continue
		}
		t.Run(command.Command, func(t *testing.T) {
			args := append(strings.Fields(command.Command), "--json", "--unsupported-flag")
			output, err := invokeFixture(t, args...)
			if err == nil {
				t.Fatalf("invalid command succeeded: %v", args)
			}
			result := decodeResult(t, output)
			if result.Error == nil || result.Error.ExitCode != 1 || result.Status != "failed" {
				t.Fatalf("failure contract: %s", output)
			}
		})
	}
	for _, args := range [][]string{{"unknown", "--json"}, {"help", "unknown", "--json"}, {"--json"}} {
		output, err := invokeFixture(t, args...)
		if err == nil {
			t.Fatalf("invalid command succeeded: %v", args)
		}
		decodeResult(t, output)
	}
}

func TestCLIJSONVersion(t *testing.T) {
	output, err := invokeFixture(t, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output)
	if result.Data.(map[string]any)["version"] != agentsdk.Version {
		t.Fatalf("version: %s", output)
	}
}

func TestCLIOriginalOutputPreservedForTTYDetection(t *testing.T) {
	original := os.Stdout
	redirect, err := os.CreateTemp(t.TempDir(), "diagnostics-*")
	if err != nil {
		t.Fatal(err)
	}
	defer redirect.Close()
	invocationOutput = &cliResult{sourceStdout: original}
	os.Stdout = redirect
	defer func() { os.Stdout = original; invocationOutput = nil }()
	if cliOriginalStdout() != original {
		t.Fatal("JSON diagnostic routing changed login TTY/default-wait detection")
	}
}

func TestCLIHumanActionsUseOnlyValidatedFriendlySelection(t *testing.T) {
	for _, mode := range []string{"exact", "other app", "other preview", "no binding"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bound := agentRemoteBinding{AirlockURL: "https://api.example.test", AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}}
			if mode == "other app" {
				bound.AgentID = previewWorkspaceID
			}
			if mode == "other preview" {
				bound.Previews[testBuildAgentID] = previewReference{DeploymentID: previewWorkspaceID, Name: "another-preview"}
			}
			binding := agentBinding{}
			if mode != "no binding" {
				binding.putRemote("corp", bound)
			}
			if mode != "no binding" {
				if err := writeAgentBinding(dir, binding); err != nil {
					t.Fatal(err)
				}
			}
			s := selectedDeployment{flags: targetFlags{dir: dir}, baseURL: "https://api.example.test", remoteName: "corp", app: agentRemoteBinding{AgentID: testBuildAgentID, Slug: "maxframe"}, info: &airlockv1.DeploymentInfo{Id: previewRuntimeID, Name: previewName, Kind: "personal_preview"}}
			human := shellCommand(humanSelectedAction(s, "status", "--wait"))
			machine := shellCommand(selectedAction(s, "status", "", "--wait"))
			if !strings.Contains(machine, testBuildAgentID) || !strings.Contains(machine, previewName) || !strings.Contains(human, dir) {
				t.Fatalf("lost identity/context: %s / %s", human, machine)
			}
			if mode == "exact" && (strings.Contains(human, "--agent") || !strings.Contains(human, "--target preview ")) {
				t.Fatal(human)
			}
			if mode == "other preview" && (strings.Contains(human, "--agent") || !strings.Contains(human, "--target "+previewName)) {
				t.Fatal(human)
			}
			if (mode == "other app" || mode == "no binding") && (!strings.Contains(human, "--agent maxframe") || !strings.Contains(human, "--target "+previewName)) {
				t.Fatal(human)
			}
		})
	}
}

func TestCLIHelpLauncherSurvivesGoToolInvocation(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.test/app\n\ngo 1.26\n\ntool github.com/airlockrun/agentsdk/cmd/air\n")
	temporary := "/tmp/go-build123/b001/exe/air"
	action := helpActionFor(temporary, dir, "preview")
	if action.Executable != "go" || !reflect.DeepEqual(action.Args, []string{"-C", dir, "tool", "air", "help", "preview"}) || strings.Contains(shellCommand(action), temporary) {
		t.Fatalf("ephemeral launcher: %+v", action)
	}
	outside := t.TempDir()
	if action := helpActionFor(temporary, outside, "preview"); action.Executable != "" {
		t.Fatalf("invented outside-module launcher: %+v", action)
	}
	installed := filepath.Join(outside, "air")
	mustWrite(t, installed, "installed executable fixture")
	if err := os.Chmod(installed, 0755); err != nil {
		t.Fatal(err)
	}
	action = helpActionFor(installed, outside, "status")
	if action.Executable != installed || !reflect.DeepEqual(action.Args, []string{"help", "status"}) {
		t.Fatalf("installed launcher: %+v", action)
	}
}

func TestCLIHelpFlagMeaningAndRequirements(t *testing.T) {
	for _, command := range publicCommands {
		for _, flag := range declaredFlags(command) {
			if flag.Description == "" {
				t.Fatalf("undocumented %s %s", command.Command, flag.Name)
			}
		}
	}
	output, err := invokeFixture(t, "preview", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--launch", "Does not imply --wait", "DANGER", "--create", "First preview", "Existing app", "--bind", "--fixture"} {
		if !strings.Contains(output, text) {
			t.Fatalf("missing %q: %s", text, output)
		}
	}
	machine, err := invokeFixture(t, "help", "deploy", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(machine, `"description"`) || !strings.Contains(machine, `"requiredWhen"`) || !strings.Contains(machine, `"default":"10m"`) {
		t.Fatal(machine)
	}
}

func TestCLIAdmissionFailurePreservesAcceptedEffects(t *testing.T) {
	t.Cleanup(func() { invocationOutput = nil })
	for _, err := range []error{&httpStatusError{StatusCode: 403}, &staleSourceError{statusCode: 412}, &approvalRequiredError{id: fixtureJobID}} {
		invocationOutput = &cliResult{Operation: &cliOperation{Admission: "accepted", TaskID: previewTaskID}}
		if got := admissionFailure(err); got != err || invocationOutput.Operation.Admission != "accepted" || invocationOutput.Operation.TaskID != previewTaskID {
			t.Fatalf("accepted effect changed: %+v", invocationOutput.Operation)
		}
	}
	invocationOutput = nil
}

func TestCLIStatusJourneys(t *testing.T) {
	for _, test := range []struct {
		name, task, want string
		wait             bool
	}{
		{"ready", "", "Ready", false},
		{"failed update older runtime", "failed", "current version is still running", false},
		{"pending update older runtime", "running", "Building update", false},
		{"timeout", "running", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			dir := t.TempDir()
			mutations, reads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					t.Errorf("observation mutation: %s", r.Method)
				}
				reads++
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe","name":"MaxFrame"}}`, testBuildAgentID)
				case "/api/v1/agents/" + testBuildAgentID + "/deployments/production":
					candidate := ""
					if test.task != "" {
						candidate = previewWorkspaceID
					}
					fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"production","name":"production","status":"active","running":true,"imageRef":"older@sha256:123","sourceRevisionId":%q,"pendingCandidateRevisionId":%q,"buildTaskStatus":%q,"buildTaskId":%q,"routeBaseUrl":"https://maxframe.example.test"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, testSourceRevision, candidate, test.task, previewTaskID)
				case "/api/v1/tasks/" + previewTaskID:
					fmt.Fprintf(w, `{"id":%q,"kind":"agent_deploy","status":"running"}`, previewTaskID)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "developer@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			binding := agentBinding{}
			binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			args := []string{"status", dir, "--remote=corp", "--target=production"}
			if test.wait {
				args = append(args, "--wait", "--timeout=20ms")
			}
			machine, err := invokeFixture(t, append(args, "--json")...)
			result := decodeResult(t, machine)
			if test.wait {
				if !errors.Is(err, context.DeadlineExceeded) || result.Status != "timed_out" || result.Error.Code != "observation_timeout" {
					t.Fatalf("timeout: %v %s", err, machine)
				}
				if !strings.Contains(machine, "older@sha256:123") || !strings.Contains(machine, "--target") {
					t.Fatalf("lost snapshot/selection: %s", machine)
				}
			} else {
				if err != nil || result.Status != "completed" {
					t.Fatalf("snapshot: %v %s", err, machine)
				}
				human, err := invokeFixture(t, args...)
				if err != nil || !strings.Contains(human, "MaxFrame · production · corp") || !strings.Contains(human, test.want) || strings.Contains(human, testBuildAgentID) || strings.Contains(human, server.URL) {
					t.Fatalf("human: %v %s", err, human)
				}
				t.Logf("human stdout:\n%s", human)
			}
			if mutations != 0 || reads == 0 {
				t.Fatalf("unexpected effects: %d mutations, %d reads", mutations, reads)
			}
			t.Logf("JSON stdout:\n%s", machine)
		})
	}
}

func TestCLIResponseEncodingAndShellActions(t *testing.T) {
	for _, test := range []struct {
		body     string
		encoding string
	}{{"hello\n", "utf-8"}, {"\xff\x00\xfe", "base64"}} {
		t.Run(test.encoding, func(t *testing.T) {
			result := &cliResult{}
			invocationOutput = result
			t.Cleanup(func() { invocationOutput = nil })
			if err := writeResponseBody(strings.NewReader(test.body), 503, "text/plain", true); err != nil {
				t.Fatal(err)
			}
			var body responseBody
			if err := json.Unmarshal(result.Data.(json.RawMessage), &body); err != nil {
				t.Fatal(err)
			}
			if body.Encoding != test.encoding || body.StatusCode != 503 {
				t.Fatalf("body: %#v", body)
			}
		})
	}
	s := selectedDeployment{flags: targetFlags{dir: "directory with 'quotes'", url: "https://explicit.example"}, remoteName: "team_name", app: agentRemoteBinding{AgentID: testBuildAgentID}, info: &airlockv1.DeploymentInfo{Name: previewName}}
	action := selectedAction(s, "status", "Observe", "--wait")
	if !reflect.DeepEqual(action.Args[:5], []string{"-C", "directory with 'quotes'", "tool", "air", "status"}) || !strings.Contains(shellCommand(action), "'directory with '\"'\"'quotes'\"'\"''") || !strings.Contains(shellCommand(action), previewName) {
		t.Fatalf("unsafe action: %#v %s", action, shellCommand(action))
	}
	if got := normalizeOptions([]string{"hosts", "exec", "--url=https://example.test", "--", "echo", "--name=value"}); !reflect.DeepEqual(got, []string{"hosts", "exec", "--url", "https://example.test", "--", "echo", "--name=value"}) {
		t.Fatalf("payload changed: %v", got)
	}
}

func TestCLIOutputFlagsPreserveValuesAndPayloads(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "continue", "definition", "session", "--prompt", "--json"},
		{"agent", "continue", "definition", "session", "--prompt=--json"},
		{"hosts", "exec", fixtureHostID, "--", "echo", "--json"},
	} {
		clean, requested := extractJSONOption(args)
		if requested || !reflect.DeepEqual(clean, args) {
			t.Fatalf("payload changed: %v -> %v, json=%t", args, clean, requested)
		}
	}
	if _, help := helpRequest([]string{"request", "GET", "/", "--data", "--help"}); help {
		t.Fatal("help consumed a payload value")
	}
	if _, help := helpRequest([]string{"request", "GET", "/", "--data=--help"}); help {
		t.Fatal("help consumed an equals payload value")
	}
	if body := encodedBody([]byte("ASCII binary"), 200, "application/octet-stream"); body.Encoding != "base64" {
		t.Fatal("binary content type did not use base64")
	}
	if got := normalizeOptions([]string{"agent", "continue", "definition", "session", "--prompt", "--literal=value"}); !reflect.DeepEqual(got, []string{"agent", "continue", "definition", "session", "--prompt", "--literal=value"}) {
		t.Fatalf("value was reinterpreted as options: %v", got)
	}
	if got := normalizeOptions([]string{"status", "--wait=false"}); !reflect.DeepEqual(got, []string{"status", "--wait=false"}) {
		t.Fatalf("boolean value became an unintended positional: %v", got)
	}
}

func TestCLIDiscoveryFlagsAndReservedRouting(t *testing.T) {
	for _, command := range publicCommands {
		for _, flag := range declaredFlags(command) {
			if flag.Name == "" || flag.TakesValue != (flag.ValueSyntax != "") {
				t.Fatalf("inconsistent public flag: %s %#v", command.Command, flag)
			}
		}
	}
	for _, args := range [][]string{
		{"deploy", "list", "-m", "must not upload"},
		{"deploy", "status", "--message=must-not-upload"},
		{"status", "--wait=false"}, {"env", "list", "--input=ignored"},
		{"tools", "list", "--input={}"}, {"agent", "unknown"},
		{"init", "-x"}, {"build", "-x"}, {"test", "status"},
	} {
		output, err := invokeFixture(t, append(args, "--json")...)
		if err == nil {
			t.Fatalf("invalid/reserved syntax executed: %v", args)
		}
		result := decodeResult(t, output)
		if result.Error == nil {
			t.Fatalf("missing parse error: %s", output)
		}
	}
}

func TestCLIJSONLocalTestSeparatesSubprocessOutput(t *testing.T) {
	t.Setenv("GOWORK", "off")
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "go.mod"), "module cli-local-fixture\n\ngo 1.26\n")
			mustWrite(t, filepath.Join(dir, "fixture_test.go"), fmt.Sprintf("package fixture\nimport (\"fmt\"; \"testing\")\nfunc TestFixture(t *testing.T) { fmt.Println(\"child-stdout-not-a-result\"); if %t { t.Fatal(\"fixture failure\") } }\n", fail))
			var output string
			var commandErr error
			stderr := captureCommandStderr(t, func() error { output, commandErr = invokeFixture(t, "test", dir, "--json"); return nil })
			result := decodeResult(t, output)
			want := "passed"
			if fail {
				want = "failed"
			}
			if (commandErr != nil) != fail || result.Data.(map[string]any)["result"] != want || strings.Contains(output, "child-stdout-not-a-result") || !strings.Contains(stderr, "cli-local-fixture") {
				t.Fatalf("mixed/discarded subprocess output: error=%v stdout=%s stderr=%s", commandErr, output, stderr)
			}
			if fail && !strings.Contains(stderr, "child-stdout-not-a-result") {
				t.Fatalf("child output was discarded: %s", stderr)
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}

func TestCLIPreviewAcceptedAndFailedWaitDoNotReplay(t *testing.T) {
	for _, mode := range []string{"accepted", "failed wait", "uncertain admission", "local state failure", "human accepted", "human failed wait", "unchanged wait", "hosted task", "hosted unchanged"} {
		t.Run(mode, func(t *testing.T) {
			wait := mode == "failed wait" || mode == "human failed wait" || mode == "unchanged wait"
			unchanged := mode == "unchanged wait" || mode == "hosted unchanged"
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			if strings.HasPrefix(mode, "hosted") {
				t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
				t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
				t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
				t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
				t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
				t.Setenv("AIRLOCK_DEVELOPER_RUN_ID", previewTaskID)
				t.Setenv("AIRLOCK_DEPLOYMENT_SOURCE_ETAG", "")
				t.Setenv("AIRLOCK_TOKEN", testAccessToken)
			}
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "go.mod"), "module candidate\n")
			uploads := 0
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe","name":"MaxFrame"}}`, testBuildAgentID)
				case "/api/v1/agent-sdk":
					fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
				case prefix + "preview", prefix + previewName:
					if unchanged {
						fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"personal_preview","name":%q,"ownerUserId":%q,"ownerWorkspaceId":%q,"status":"active","running":true,"imageRef":"older@sha256:123","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":"failed"}}`, previewRuntimeID, testBuildAgentID, previewRuntimeID, previewName, previewOwnerID, previewWorkspaceID, testSourceRevision, previewTaskID)
						return
					}
					fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"personal_preview","name":%q,"ownerUserId":%q,"ownerWorkspaceId":%q,"status":"active","running":true,"imageRef":"older@sha256:123","routeBaseUrl":"https://maxframe-preview.example.test"}}`, previewRuntimeID, testBuildAgentID, previewRuntimeID, previewName, previewOwnerID, previewWorkspaceID)
				case prefix + previewName + "/source":
					if r.Method != http.MethodPut {
						t.Errorf("unexpected source method %s", r.Method)
					}
					uploads++
					state, err := sourcebundle.ExtractArchiveState(r.Body, t.TempDir())
					if err != nil {
						t.Error(err)
					}
					if mode == "uncertain admission" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":"fixture uncertain source admission"}`)
						return
					}
					if mode == "local state failure" {
						bindingPath := filepath.Join(dir, agentBindingPath)
						if err := os.Remove(bindingPath); err != nil {
							t.Error(err)
						}
						if err := os.Mkdir(bindingPath, 0o700); err != nil {
							t.Error(err)
						}
					}
					w.Header().Set(wire.SourceStateHeader, state)
					w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
					w.Header().Set(wire.SourceGenerationHeader, "2")
					if !unchanged {
						w.Header().Set(wire.SourceTaskHeader, previewTaskID)
						w.WriteHeader(http.StatusAccepted)
					} else {
						w.WriteHeader(http.StatusOK)
					}
				case "/api/v1/tasks/" + previewTaskID:
					fmt.Fprintf(w, `{"id":%q,"status":"failed","kind":"agent_deploy"}`, previewTaskID)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if strings.HasPrefix(mode, "hosted") {
				t.Setenv("AIRLOCK_API_URL", server.URL)
			}
			if err := saveLoginCredentials(server.URL, "developer@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			binding := agentBinding{}
			binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			args := []string{"preview", dir, "-m", "Candidate"}
			if !strings.HasPrefix(mode, "human") {
				args = append(args, "--json")
			}
			if wait {
				args = append(args, "--wait")
			}
			output, err := invokeFixture(t, args...)
			if strings.HasPrefix(mode, "human") {
				if uploads != 1 || !strings.Contains(output, "MaxFrame · Preview · corp") || !strings.Contains(output, "Deployment submitted.") || strings.Contains(output, "Runtime:") || strings.Contains(output, "Candidate:") || strings.Contains(output, server.URL) || strings.Contains(output, testBuildAgentID) || strings.Contains(output, previewName) || strings.Contains(output, "--timeout 10m0s") {
					t.Fatalf("human submission: %v %s", err, output)
				}
				if wait && (err == nil || !strings.Contains(output, "Observed update failed") || !strings.Contains(output, "current version is still running")) {
					t.Fatalf("failed attempt hid serving runtime: %v %s", err, output)
				}
				if !wait && err != nil {
					t.Fatal(err)
				}
				t.Logf("human stdout:\n%s", output)
				return
			}
			result := decodeResult(t, output)
			if uploads != 1 || !strings.Contains(output, "older@sha256:123") {
				t.Fatalf("lost operation or replayed: uploads=%d %s", uploads, output)
			}
			if mode == "uncertain admission" {
				if err == nil || result.Error.Code != "effect_unknown" || result.Operation.Admission != "unknown" || result.Operation.UploadedSourceState == "" || result.Operation.RuntimeID != previewRuntimeID || result.Operation.TaskID != "" || !strings.Contains(output, "inspect") {
					t.Fatalf("uncertain source handle lost or fabricated: %v %s", err, output)
				}
			} else if !strings.Contains(output, testSourceRevision) || (!unchanged && !strings.Contains(output, previewTaskID)) {
				t.Fatalf("accepted operation lost: %s", output)
			} else if mode == "local state failure" {
				if err == nil || result.Operation.Admission != "accepted" || result.Operation.TaskID != previewTaskID || !strings.Contains(result.Error.Message, "source admitted (task "+previewTaskID+")") || !strings.Contains(result.Error.Message, "without resubmitting") {
					t.Fatalf("accepted task lost after local persistence failure: %v %s", err, output)
				}
			} else if mode == "unchanged wait" {
				if err != nil || result.Status != "completed" || result.Operation.TaskID != "" || !strings.Contains(output, `"buildTaskStatus":"failed"`) {
					t.Fatalf("historical task invalidated accepted-source readiness: %v %s", err, output)
				}
			} else if wait {
				if err == nil || result.Status != "failed" || result.Error == nil {
					t.Fatalf("failed attempt: %v %s", err, output)
				}
			} else if err != nil || result.Status != "accepted" || result.Error != nil {
				t.Fatalf("acceptance: %v %s", err, output)
			}
			if mode == "hosted task" && (len(result.Guidance) != 2 || !strings.Contains(output, `"--wait"`) || strings.Contains(output, `"when"`)) {
				t.Fatalf("hosted task guidance: %s", output)
			}
			if mode == "hosted unchanged" && (result.Operation.TaskID != "" || len(result.Guidance) != 2 || !strings.Contains(output, `"--revision"`) || !strings.Contains(output, `"--wait"`)) {
				t.Fatalf("hosted no-task guidance: %s", output)
			}
			if strings.Contains(output, testAccessToken) {
				t.Fatal("credential leaked")
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}

func TestCLILoginPendingJSONOmitsCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_TOKEN", "")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/device/begin" {
			t.Errorf("unexpected login request: %s", r.URL)
		}
		fmt.Fprint(w, `{"deviceCode":"private-device-code","userCode":"ABCD-EFGH","verificationUrl":"https://login.example.test/device","expiresInSeconds":600,"pollIntervalSeconds":3}`)
	}))
	defer server.Close()
	output, err := invokeFixture(t, "login", server.URL, "--no-browser", "--json")
	if err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output)
	if result.Status != "pending" || !strings.Contains(output, "ABCD-EFGH") || !strings.Contains(output, "--check") || strings.Contains(output, "private-device-code") {
		t.Fatalf("login contract: %s", output)
	}
	t.Logf("JSON stdout:\n%s", output)
}

func TestCLIHostJSONRetainsRecoveryAndExit(t *testing.T) {
	for _, status := range []int{200, 403, 428, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			t.Setenv("SOL_COMMAND_CONTROL_SOCKET", "")
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/hosts/"+fixtureHostID+"/shell" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				posts++
				if status != 200 {
					if status == 428 {
						w.Header().Set("Airlock-Operation-Challenge", fixtureJobID)
					}
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":"fixture denial"}`)
					return
				}
				writeConnectorProto(t, w, &airlockv1.HostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, Id: fixtureJobID, HostId: fixtureHostID, Kind: "shell", Status: "succeeded", OutputJson: `{"stdout":"//4=","stderr":"ZXJyb3I=","exitCode":23}`}})
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			output, err := invokeFixture(t, "hosts", "exec", fixtureHostID, "--url", server.URL, "--request-id", fixtureJobID, "--json", "--", "fixture-not-executed")
			result := decodeResult(t, output)
			if err == nil || result.Error == nil || posts != 1 || !strings.Contains(output, fixtureJobID) || !strings.Contains(output, "Recover this exact request") {
				t.Fatalf("recovery: %v posts=%d %s", err, posts, output)
			}
			if status == 200 && (result.Error.ExitCode != 23 || !strings.Contains(output, `"encoding":"base64"`)) {
				t.Fatalf("remote shell exit/bytes: %s", output)
			}
			if status == 403 && result.Error.Code != "permission_denied" {
				t.Fatalf("permission: %s", output)
			}
			if status == 503 && (result.Error.Code != "effect_unknown" || result.Operation.Admission != "unknown") {
				t.Fatalf("uncertain admission became an execution result: %s", output)
			}
			if status == 428 && (result.Error.ExitCode != 77 || result.Error.ApprovalID != fixtureJobID || result.Status != "pending" || result.Operation.RequestID != fixtureJobID || result.Operation.Admission != "blocked") {
				t.Fatalf("approval lost request: %s", output)
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}

func TestCLIReadinessWaitPinsTaskWithoutRebasingSource(t *testing.T) {
	for _, outcome := range []string{"ready", "task changed", "candidate changed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			dir := t.TempDir()
			runtimeReads, mutations := 0, 0
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
				}
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe"}}`, testBuildAgentID)
				case prefix:
					runtimeReads++
					task, candidate, status, revision := previewTaskID, previewWorkspaceID, "running", testSourceRevision
					if runtimeReads > 1 {
						switch outcome {
						case "ready":
							candidate, status, revision = "", "succeeded", previewWorkspaceID
						case "task changed":
							task = previewWorkspaceID
						case "candidate changed":
							candidate = testBuildID
						}
					}
					fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"production","name":"production","status":"active","running":true,"imageRef":"observed-image","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":%q,"pendingCandidateRevisionId":%q}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, revision, task, status, candidate)
				default:
					t.Errorf("unexpected task reconciliation/source write: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "developer@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			base := targetSourceBinding{Name: "production", SourceState: "sha256:local-base", SourceETag: (wire.SourceVersion{Revision: testSourceRevision, Generation: 1}).ETag()}
			binding := agentBinding{}
			binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Targets: map[string]targetSourceBinding{testBuildAgentID: base}})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			output, err := invokeFixture(t, "status", "--dir", dir, "--wait", "--timeout", "1s", "--json")
			result := decodeResult(t, output)
			if outcome == "ready" && (err != nil || result.Status != "completed") {
				t.Fatalf("readiness: %v %s", err, output)
			}
			if outcome != "ready" && (err == nil || !strings.Contains(err.Error(), "changed while observing readiness")) {
				t.Fatalf("changed observation accepted: %v %s", err, output)
			}
			updated, _, err := loadAgentBinding(dir)
			if err != nil {
				t.Fatal(err)
			}
			remote, _ := updated.remote("corp")
			if !reflect.DeepEqual(remote.Targets[testBuildAgentID], base) || mutations != 0 || runtimeReads != 2 {
				t.Fatalf("observation rebased source or mutated: before=%+v after=%+v writes=%d reads=%d", base, remote.Targets[testBuildAgentID], mutations, runtimeReads)
			}
		})
	}
}

func TestCLIServingRevisionGuardDoesNotReconcileOrMutate(t *testing.T) {
	for _, command := range []string{"status", "inspect"} {
		for _, revision := range []string{testSourceRevision, previewWorkspaceID} {
			t.Run(command+"/"+revision, func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
				dir := t.TempDir()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("observation mutated: %s %s", r.Method, r.URL)
					}
					switch r.URL.Path {
					case "/api/v1/agents/" + testBuildAgentID:
						fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe"}}`, testBuildAgentID)
					case "/api/v1/agents/" + testBuildAgentID + "/deployments/production":
						fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"kind":"production","name":"production","status":"active","running":true,"imageRef":"serving-image","sourceRevisionId":%q,"buildTaskId":%q,"buildTaskStatus":"failed"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, testSourceRevision, previewTaskID)
					default:
						t.Errorf("unexpected reconciliation: %s", r.URL)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				if err := saveLoginCredentials(server.URL, "developer@example.test", testAccessToken, ""); err != nil {
					t.Fatal(err)
				}
				base := targetSourceBinding{Name: "production", PendingTask: previewTaskID, PendingSourceState: "sha256:own-pending"}
				binding := agentBinding{}
				binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Targets: map[string]targetSourceBinding{testBuildAgentID: base}})
				if err := writeAgentBinding(dir, binding); err != nil {
					t.Fatal(err)
				}
				args := []string{command, "--dir", dir, "--revision", revision, "--json"}
				if command == "status" {
					args = append(args, "--wait", "--timeout", "1s")
				}
				output, err := invokeFixture(t, args...)
				result := decodeResult(t, output)
				if revision == testSourceRevision && (err != nil || result.Status != "completed") {
					t.Fatalf("serving revision rejected: %v %s", err, output)
				}
				if revision != testSourceRevision && (err == nil || !strings.Contains(err.Error(), "source changed")) {
					t.Fatalf("revision mismatch accepted: %v %s", err, output)
				}
				updated, _, err := loadAgentBinding(dir)
				if err != nil {
					t.Fatal(err)
				}
				remote, _ := updated.remote("corp")
				if !reflect.DeepEqual(remote.Targets[testBuildAgentID], base) {
					t.Fatal("revision observation rebased source")
				}
				t.Logf("JSON stdout:\n%s", output)
			})
		}
	}
}

func TestCLIBuildWaitJSONOneDocument(t *testing.T) {
	for _, terminal := range []string{"complete", "failed", "timeout"} {
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			dir := t.TempDir()
			observations, mutations := 0, 0
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
				}
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"name":"MaxFrame","slug":"maxframe"}}`, testBuildAgentID)
				case prefix:
					fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production","status":"active","running":true,"imageRef":"older-image","routeBaseUrl":"https://maxframe.example.test","workspaceUrl":"https://dashboard.example.test/chat?app=maxframe"}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID)
				case prefix + "/builds/" + testBuildID:
					observations++
					status := "building"
					if terminal == "failed" || observations > 1 && terminal != "timeout" {
						status = terminal
					}
					fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":%q}}`, testBuildID, testBuildAgentID, status)
				default:
					t.Errorf("unexpected build request: %s", r.URL)
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
			timeout := "5s"
			if terminal == "timeout" {
				timeout = "20ms"
			}
			output, err := invokeFixture(t, "deploy", "status", dir, "--build", testBuildID, "--wait", "--timeout", timeout, "--json")
			result := decodeResult(t, output)
			if terminal == "complete" && (err != nil || result.Status != "completed" || observations != 2) {
				t.Fatalf("completion: %v count=%d %s", err, observations, output)
			}
			if terminal == "failed" && (err == nil || result.Status != "failed" || result.Error == nil) {
				t.Fatalf("failure: %v %s", err, output)
			}
			if terminal == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || result.Status != "timed_out" || !strings.Contains(output, "building")) {
				t.Fatalf("timeout: %v %s", err, output)
			}
			if mutations != 0 || result.Data.(map[string]any)["kind"] != "build" {
				t.Fatalf("build observation changed state or shape: %s", output)
			}
			t.Logf("JSON stdout:\n%s", output)
		})
	}
}

func TestCLIRequestRawAndJSONBody(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	dir := t.TempDir()
	body := "\xff\x00binary-body"
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"maxframe","name":"MaxFrame"}}`, testBuildAgentID)
		case prefix:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production","sourceRevisionId":%q}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, testSourceRevision)
		case prefix + "/http/blob":
			requests++
			if r.Method != http.MethodGet || r.URL.RawQuery != "q=x" || r.Header.Get(wire.ExpectedSourceRevisionHeader) != testSourceRevision || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
				t.Errorf("request admission/payload changed: %s %s", r.Method, r.URL)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(503)
			fmt.Fprint(w, body)
		default:
			t.Errorf("unexpected HTTP request: %s", r.URL)
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
	args := []string{"request", "GET", "/blob?q=x", "--dir", dir}
	raw, err := invokeFixture(t, args...)
	if err == nil || raw != body {
		t.Fatalf("raw stdout changed: %v %q", err, raw)
	}
	output, err := invokeFixture(t, append(args, "--json")...)
	result := decodeResult(t, output)
	data := result.Data.(map[string]any)
	if err == nil || result.Error == nil || data["statusCode"] != float64(503) || data["encoding"] != "base64" || data["body"] != base64.StdEncoding.EncodeToString([]byte(body)) || data["contentType"] != "application/octet-stream" || requests != 2 {
		t.Fatalf("HTTP JSON wrapper: %v %s", err, output)
	}
	t.Logf("JSON stdout:\n%s", output)
}

func TestCLIToolInvocationHumanAndJSON(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	dir := t.TempDir()
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/production"
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"name":"MaxFrame","slug":"maxframe"}}`, testBuildAgentID)
		case prefix:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production","sourceRevisionId":%q}}`, testBuildAgentID, testBuildAgentID, testBuildAgentID, testSourceRevision)
		case prefix + "/tools/report/invoke":
			posts++
			if r.Method != http.MethodPost || r.Header.Get(wire.ExpectedSourceRevisionHeader) != testSourceRevision {
				t.Error("tool lost method/revision fence")
			}
			fmt.Fprintf(w, `{"runId":%q,"resultJson":"{\"count\":9007199254740993,\"title\":\"Report\"}"}`, previewTaskID)
		default:
			t.Errorf("unexpected tool request: %s", r.URL)
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
	args := []string{"tools", "invoke", "report", "--dir", dir, "--input", `{}`}
	human, err := invokeFixture(t, args...)
	if err != nil || !strings.Contains(human, "count: 9007199254740993") || !strings.Contains(human, "title: Report") || strings.Contains(human, "resultJson") || strings.Contains(human, `{"`) {
		t.Fatalf("human tool result: %v %s", err, human)
	}
	machine, err := invokeFixture(t, append(args, "--json")...)
	result := decodeResult(t, machine)
	if err != nil || result.Data.(map[string]any)["runId"] != previewTaskID || result.Data.(map[string]any)["result"].(map[string]any)["title"] != "Report" || !strings.Contains(machine, `"count":9007199254740993`) || posts != 2 {
		t.Fatalf("tool JSON result lost precision or replayed: %v %s", err, machine)
	}
	t.Logf("human stdout:\n%s", human)
	t.Logf("JSON stdout:\n%s", machine)
}

func TestCLIResourceToolsHumanAndMalformedJSON(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%t", malformed), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			dir := t.TempDir()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("resource discovery mutated state")
				}
				switch r.URL.Path {
				case "/api/v1/resources/resolve/fixture_mcp":
					fmt.Fprintf(w, `{"id":%q,"type":"mcp_server","slug":"fixture_mcp","displayName":"Fixture MCP"}`, fixtureHostID)
				case "/api/v1/resources/mcp_servers/" + fixtureHostID + "/tools":
					schema := []byte(`{"type":"object"}`)
					if malformed {
						schema = []byte(`{`)
					}
					writeConnectorProto(t, w, &airlockv1.ListIntegrationMCPToolsResponse{Instructions: "Approved resource tools", Tools: []*airlockv1.IntegrationMCPTool{{Name: "report", Description: "Create a report", InputSchemaJson: schema}}})
				default:
					t.Errorf("global discovery depended on deployment/app: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.test", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			binding := agentBinding{}
			binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			args := []string{"resources", "mcp-tools", "/fixture_mcp", "--dir", dir}
			human, humanErr := invokeFixture(t, args...)
			machine, machineErr := invokeFixture(t, append(args, "--json")...)
			result := decodeResult(t, machine)
			if malformed {
				if humanErr == nil || machineErr == nil || result.Error == nil || result.Status != "failed" {
					t.Fatalf("malformed schema escaped result validation: human=%v machine=%v %s", humanErr, machineErr, machine)
				}
			} else if humanErr != nil || machineErr != nil || !strings.Contains(human, "report") || !strings.Contains(human, "Create a report") || strings.Contains(human, "inputSchema") || strings.Contains(human, fixtureHostID) || !strings.Contains(machine, `"inputSchema":{"type":"object"}`) {
				t.Fatalf("MCP discovery rendering: human=%v %s machine=%v %s", humanErr, human, machineErr, machine)
			}
			t.Logf("human stdout:\n%s", human)
			t.Logf("JSON stdout:\n%s", machine)
		})
	}
}
