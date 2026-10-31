package main

import (
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

const previewRuntimeID = "22222222-2222-2222-2222-222222222222"
const previewOwnerID = "55555555-5555-5555-5555-555555555555"
const previewTaskID = "66666666-6666-6666-6666-666666666666"
const previewWorkspaceID = "77777777-7777-7777-7777-777777777777"
const previewName = "p-0123456789abcdef0123456789abcdef"

func TestPreviewUploadsCompleteCandidateAndScopesSource(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module candidate\n")
	mustWrite(t, filepath.Join(dir, "integration", "app_test.go"), "//go:build airlockintegration\n\npackage integration\n")
	mustWrite(t, filepath.Join(dir, "testdata", "items.json"), `[{"id":1}]`)
	mustWrite(t, filepath.Join(dir, "preview-fixtures.json"), `{"version":1,"fixtures":[]}`)
	var paths []string
	var archiveState string
	version := wire.SourceVersion{Revision: testSourceRevision, Generation: 7}
	appURL := "https://exact-preview.server-selected.example.test"
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/preview"
	concretePrefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Errorf("wrong credential")
		}
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"candidate"}}`, testBuildAgentID)
		case "/api/v1/agent-sdk":
			fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
		case prefix, concretePrefix:
			body, err := protoMarshal.Marshal(&airlockv1.GetDeploymentResponse{Deployment: &airlockv1.DeploymentInfo{
				Id: previewRuntimeID, AppId: testBuildAgentID, RuntimeAgentId: previewRuntimeID, Kind: "personal_preview", Name: previewName, OwnerUserId: previewOwnerID,
				Status: "active", Running: boolPointer(true), ImageRef: "candidate@sha256:123", SourceRevisionId: testSourceRevision,
				RouteBaseUrl: appURL, WorkspaceUrl: "https://dashboard.example.test/chat/" + previewWorkspaceID,
			}})
			if err != nil {
				t.Error(err)
			}
			_, _ = w.Write(body)
		case concretePrefix + "/source":
			if r.Method == http.MethodPut {
				if !strings.Contains(r.Header.Get(wire.CandidateConfigHeader), `"fixtureName":"canned-crm"`) {
					t.Error("fixture plan was not attached atomically to source PUT")
				}
				if r.Header.Get("If-Match") != "" {
					t.Errorf("preview inherited production ETag")
				}
				extracted := t.TempDir()
				var err error
				archiveState, err = sourcebundle.ExtractArchiveState(r.Body, extracted)
				if err != nil {
					t.Error(err)
				}
				for _, name := range []string{"go.mod", "integration/app_test.go", "testdata/items.json", "preview-fixtures.json"} {
					if _, err := os.Stat(filepath.Join(extracted, name)); err != nil {
						t.Errorf("missing candidate file %s: %v", name, err)
					}
				}
				if _, err := os.Stat(filepath.Join(extracted, agentBindingPath)); !os.IsNotExist(err) {
					t.Error("local state entered archive")
				}
			}
			w.Header().Set("ETag", quoteETag(version.ETag()))
			w.Header().Set(wire.SourceStateHeader, archiveState)
			w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
			w.Header().Set(wire.SourceGenerationHeader, "7")
			if r.Method == http.MethodPut {
				w.Header().Set(wire.SourceTaskHeader, previewTaskID)
				w.WriteHeader(http.StatusAccepted)
			}
		case "/api/v1/tasks/" + previewTaskID:
			w.Header().Set("X-Airlock-Build-ID", testBuildID)
			fmt.Fprintf(w, `{"id":%q,"status":"succeeded","kind":"agent_deploy"}`, previewTaskID)
		case concretePrefix + "/builds/" + testBuildID:
			fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"complete"}}`, testBuildID, previewRuntimeID)
		default:
			t.Errorf("unexpected API %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "developer@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	binding := agentBinding{}
	binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, SourceETag: "production-etag", Targets: map[string]targetSourceBinding{
		testBuildAgentID: {Name: "production", SourceETag: "production-target-etag", SourceState: "production-state"},
	}})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	out, err := captureCommandStdoutResult(t, func() error {
		return cmdPreview([]string{dir, "--wait", "--fixture", "crm=canned-crm", "-m", "Candidate"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "App URL: "+appURL) || strings.Contains(out, "https://dashboard.example.test/chat/") {
		t.Fatalf("preview URL: %s", out)
	}
	updated, _, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := updated.remote("corp")
	preview := remote.Targets[previewRuntimeID]
	if preview.SourceETag != version.ETag() || preview.SourceState != archiveState || preview.PendingTask != "" || preview.OwnerUserID != previewOwnerID {
		t.Fatalf("preview source = %+v", preview)
	}
	if remote.Targets[testBuildAgentID].SourceState != "production-state" || remote.SourceETag != "production-etag" {
		t.Fatal("preview modified production state")
	}
	if !reflect.DeepEqual(paths[:3], []string{"GET /api/v1/agents/" + testBuildAgentID, "POST " + prefix, "GET /api/v1/agent-sdk"}) {
		t.Fatalf("paths = %v", paths)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestHostedPreviewUsesAdmittedContextWithoutRemoteOrLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
	t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
	t.Setenv("AIRLOCK_DEVELOPER_RUN_ID", previewTaskID)
	t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
	t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
	t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
	t.Setenv("AIRLOCK_TOKEN", testAccessToken)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module candidate\n")
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("request did not use the admitted token")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET " + prefix:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q,"ownerUserId":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID, previewOwnerID)
		case "GET /api/v1/agent-sdk":
			fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
		case "PUT " + prefix + "/source":
			if r.Header.Get("X-Airlock-Developer-Session") != previewWorkspaceID || r.Header.Get("X-Airlock-Developer-Run") != previewTaskID {
				t.Error("source upload did not carry the exact admitted session/run")
			}
			state, err := sourcebundle.ExtractArchiveState(r.Body, t.TempDir())
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid source", http.StatusBadRequest)
				return
			}
			w.Header().Set(wire.SourceStateHeader, state)
			w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
			w.Header().Set(wire.SourceGenerationHeader, "1")
			w.Header().Set(wire.SourceTaskHeader, previewTaskID)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected hosted API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_API_URL", server.URL)
	if _, err := captureCommandStdoutResult(t, func() error {
		return cmdPreview([]string{dir, "-m", "Hosted preview"})
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET " + prefix, "GET /api/v1/agent-sdk", "PUT " + prefix + "/source", "GET " + prefix}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("hosted preview requests = %v, want %v", paths, want)
	}
}

func TestNeverRunLocalHost(t *testing.T) {
	if err := run([]string{"run", "--local"}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("local run = %v", err)
	}
	if err := cmdPreview([]string{"--target", "production", "-m", "No fallback"}); err == nil {
		t.Fatal("preview accepted production selector")
	}
}

func TestHostedPreviewLaunchSourceAdmissionBeforeStart(t *testing.T) {
	for _, tt := range []struct {
		name, status         string
		uploadStatus, starts int
		buildFails           bool
	}{
		{"stopped available", "stopped", http.StatusOK, 1, false},
		{"suspended available", "active", http.StatusOK, 1, false},
		{"stopped missing", "stopped", http.StatusAccepted, 1, false},
		{"stopped build failure", "stopped", http.StatusAccepted, 0, true},
		{"suspended missing", "active", http.StatusAccepted, 0, false},
		{"source conflict", "stopped", http.StatusPreconditionFailed, 0, false},
		{"daemon failure", "stopped", http.StatusInternalServerError, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", previewWorkspaceID)
			t.Setenv("AIRLOCK_DEVELOPER_SESSION_ID", previewWorkspaceID)
			t.Setenv("AIRLOCK_DEVELOPER_RUN_ID", previewTaskID)
			t.Setenv("AIRLOCK_APP_ID", testBuildAgentID)
			t.Setenv("AIRLOCK_AGENT_ID", previewRuntimeID)
			t.Setenv("AIRLOCK_DEPLOYMENT_TARGET", previewName)
			t.Setenv("AIRLOCK_TOKEN", testAccessToken)
			t.Setenv("AIRLOCK_WORKSPACE_COMMAND_TASK", previewTaskID)
			t.Setenv("AIRLOCK_WORKSPACE_COMMAND_OWNER", previewOwnerID)
			version := wire.SourceVersion{Revision: testSourceRevision, Generation: 7}
			t.Setenv("AIRLOCK_DEPLOYMENT_SOURCE_ETAG", version.ETag())
			if status == "active" {
				if err := os.Unsetenv("AIRLOCK_DEPLOYMENT_SOURCE_ETAG"); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "go.mod"), "module candidate\n")
			prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
			starts, uploads, observations := 0, 0, 0
			built := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("missing admitted credential")
				}
				switch r.Method + " " + r.URL.Path {
				case "GET " + prefix:
					fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q,"ownerUserId":%q,"imageRef":"image","sourceRevisionId":%q,"status":%q,"running":false}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID, previewOwnerID, testSourceRevision, status)
				case "POST " + prefix + "/start":
					if uploads != 1 {
						t.Error("runtime start preceded source/image admission")
					}
					if tt.uploadStatus == http.StatusAccepted && !built {
						t.Error("start did not wait for the replacement build")
					}
					starts++
					w.WriteHeader(http.StatusNoContent)
				case "GET /api/v1/agent-sdk":
					fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
				case "HEAD " + prefix + "/source":
					observations++
					w.Header().Set("ETag", quoteETag(version.ETag()))
					w.Header().Set(wire.SourceStateHeader, "observed-state")
					w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
					w.Header().Set(wire.SourceGenerationHeader, "7")
				case "PUT " + prefix + "/source":
					uploads++
					if r.Header.Get("If-Match") != quoteETag(version.ETag()) {
						t.Errorf("source CAS = %q", r.Header.Get("If-Match"))
					}
					if r.Header.Get("X-Airlock-Workspace-Command") != previewTaskID || r.Header.Get("X-Airlock-Workspace-Owner") != previewOwnerID {
						t.Error("source upload lacks task fence")
					}
					state, err := sourcebundle.ExtractArchiveState(r.Body, t.TempDir())
					if err != nil {
						t.Error(err)
					}
					w.Header().Set(wire.SourceStateHeader, state)
					w.Header().Set(wire.SourceRevisionHeader, testSourceRevision)
					w.Header().Set(wire.SourceGenerationHeader, "7")
					if tt.uploadStatus == http.StatusAccepted {
						w.Header().Set(wire.SourceTaskHeader, previewTaskID)
					}
					w.WriteHeader(tt.uploadStatus)
				case "GET /api/v1/tasks/" + previewTaskID:
					if tt.buildFails {
						w.Header().Set("X-Airlock-Task-Error", "build failed")
						fmt.Fprintf(w, `{"id":%q,"status":"failed"}`, "task:"+previewTaskID)
					} else {
						built = true
						w.Header().Set("X-Airlock-Build-ID", previewTaskID)
						fmt.Fprintf(w, `{"id":%q,"status":"succeeded"}`, "task:"+previewTaskID)
					}
				case "GET " + prefix + "/builds/" + previewTaskID:
					fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"complete"}}`, previewTaskID, previewRuntimeID)
				default:
					t.Errorf("unexpected request/wait: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("AIRLOCK_API_URL", server.URL)
			_, err := captureCommandStdoutResult(t, func() error { return cmdPreview([]string{dir, "-m", "Launch", "--launch"}) })
			if (err != nil) != (tt.uploadStatus >= 400 || tt.buildFails) {
				t.Fatalf("launch error = %v", err)
			}
			if starts != tt.starts || uploads != 1 {
				t.Fatalf("start=%d upload=%d", starts, uploads)
			}
			if tt.uploadStatus == http.StatusAccepted && status == "active" {
				binding, _, err := loadAgentBinding(dir)
				if err != nil {
					t.Fatal(err)
				}
				remote, ok := binding.remote(binding.DefaultRemote)
				if !ok || remote.Targets[previewRuntimeID].PendingTask != previewTaskID {
					t.Fatalf("missing durable build handle: %+v", binding)
				}
			}
			wantObservations := 0
			if status == "active" || built {
				wantObservations = 1
			}
			if observations != wantObservations {
				t.Fatalf("unexpected source observation count %d", observations)
			}
		})
	}
}

func TestLocalTestsRejectSimulatorImportsBeforeAnyInfrastructure(t *testing.T) {
	for _, imported := range []string{"github.com/airlockrun/agentsdk/agenttest", "github.com/airlockrun/agentsdk/localruntime", "github.com/testcontainers/testcontainers-go"} {
		t.Run(imported, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "domain_test.go"), "package domain\nimport _ "+strconvQuote(imported)+"\n")
			if err := cmdTest([]string{dir}); err == nil || !strings.Contains(err.Error(), imported) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "remote_test.go"), "//go:build airlockintegration\n\npackage integration\nimport _ \"github.com/airlockrun/agentsdk/agenttest\"\n")
	if err := validateUnitTestImports(dir); err != nil {
		t.Fatal(err)
	}
}

func TestUnitCommandRunsWithNoDatabaseOrDocker(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("DOCKER_HOST", "unix:///nonexistent")
	t.Setenv("TEST_DB_URL", "")
	t.Setenv("AIRLOCK_API_URL", "")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "")
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module deterministic-fixture\n\ngo 1.26\n")
	mustWrite(t, filepath.Join(dir, "domain_test.go"), `package domain
import("os"; "testing")
func TestNoInfrastructureInjected(t *testing.T) {
 if os.Getenv("TEST_DB_URL") != "" || os.Getenv("AIRLOCK_AGENT_TOKEN") != "" || os.Getenv("AIRLOCK_API_URL") != "" { t.Fatal("host infrastructure entered local tests") }
}
`)
	if _, err := captureCommandStdoutResult(t, func() error { return cmdTest([]string{dir}) }); err != nil {
		t.Fatal(err)
	}
}

func strconvQuote(value string) string { return fmt.Sprintf("%q", value) }

func TestDirectResourcesDoNotRequireAnApp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/resources" || r.Method != "GET" {
			t.Errorf("unexpected path %s", r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"resources":[]}`)
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCommandStdoutResult(t, func() error { return cmdResources([]string{"list", "--url", server.URL, "--dir", t.TempDir()}) }); err != nil {
		t.Fatal(err)
	}
	if err := cmdResources([]string{"list", "--target", "production"}); err == nil {
		t.Fatal("direct resource operation accepted app target")
	}
}

func TestBindingStateSeparatesRemoteAndPreviewOwner(t *testing.T) {
	binding := agentBinding{DefaultRemote: "corp-a", Remotes: map[string]agentRemoteBinding{
		"corp-a": {AirlockURL: "https://a.example", AgentID: testBuildAgentID, Targets: map[string]targetSourceBinding{
			previewRuntimeID: {Name: "preview-a", OwnerUserID: previewOwnerID, SourceETag: "etag-a"},
			previewTaskID:    {Name: "preview-b", OwnerUserID: testSourceRevision, SourceETag: "etag-b"},
		}},
		"corp-b": {AirlockURL: "https://b.example", AgentID: testBuildAgentID, Targets: map[string]targetSourceBinding{
			previewRuntimeID: {Name: "preview-a", OwnerUserID: previewOwnerID, SourceETag: "etag-other-instance"},
		}},
	}}
	dir := t.TempDir()
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	actual, _, err := loadAgentBinding(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, binding) {
		t.Fatalf("roundtrip = %#v, want %#v", actual, binding)
	}
}

func TestFixtureBindingIsRevisionScopedAndServerStored(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	concretePrefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewName
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, testBuildAgentID)
		case "/api/v1/agents/" + testBuildAgentID + "/deployments/" + previewRuntimeID:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","ownerUserId":%q,"ownerWorkspaceId":%q,"name":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewOwnerID, previewWorkspaceID, previewName)
		case concretePrefix + "/bindings/connection/crm":
			puts++
			if r.Method != http.MethodPut {
				t.Errorf("method %s", r.Method)
			}
			raw, _ := io.ReadAll(r.Body)
			var request airlockv1.SetTargetBindingRequest
			if err := protoUnmarshal.Unmarshal(raw, &request); err != nil {
				t.Error(err)
			}
			if request.FixtureName != "canned-crm" || request.SourceRevisionId != testSourceRevision || request.ResourceId != "" {
				t.Errorf("fixture = %v", &request)
			}
			fmt.Fprintf(w, `{"runtimeAgentId":%q,"configRevision":"2","bindings":[]}`, previewRuntimeID)
		case concretePrefix + "/bindings":
			fmt.Fprintf(w, `{"runtimeAgentId":%q,"bindings":[{"type":"connection","slug":"crm","selection":"unbound"}]}`, previewRuntimeID)
		default:
			t.Errorf("unexpected API %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "developer@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binding := agentBinding{}
	binding.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, agentBindingPath))
	if _, err := captureCommandStdoutResult(t, func() error {
		return cmdResources([]string{"fixture", "preview/crm", "--fixture", "canned-crm", "--revision", testSourceRevision, "--dir", dir})
	}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, agentBindingPath))
	if puts != 1 || string(before) != string(after) {
		t.Fatal("fixture selection wrote a local binding or missed server PUT")
	}
}

func TestPreviewCandidateConfigurationUsesPendingRevision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	var methods []string
	prefix := "/api/v1/agents/" + testBuildAgentID + "/deployments/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("unexpected credentials")
		}
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, testBuildAgentID)
		case prefix + previewRuntimeID:
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"kind":"personal_preview","name":%q,"ownerWorkspaceId":%q}}`, previewRuntimeID, previewRuntimeID, testBuildAgentID, previewName, previewWorkspaceID)
		case prefix + previewName + "/candidates/" + testSourceRevision + "/configuration":
			methods = append(methods, r.Method)
			if r.Method == http.MethodPut {
				raw, _ := io.ReadAll(r.Body)
				var input airlockv1.SetCandidateConfigurationRequest
				if err := protoUnmarshal.Unmarshal(raw, &input); err != nil {
					t.Error(err)
				}
				if len(input.Selections) != 1 || input.Selections[0].FixtureName != "canned-crm" || input.Selections[0].Slug != "crm" || input.Selections[0].Type != "connection" {
					t.Errorf("wrong candidate plan: %v", &input)
				}
			}
			fmt.Fprintf(w, `{"sourceRevisionId":%q,"configRevision":"2"}`, testSourceRevision)
		default:
			t.Errorf("unexpected API %s", r.URL)
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
	before, _ := os.ReadFile(filepath.Join(dir, agentBindingPath))
	for _, extra := range [][]string{nil, {"--fixture", "crm=canned-crm"}} {
		args := append([]string{"configuration", testSourceRevision, "--dir", dir}, extra...)
		if _, err := captureCommandStdoutResult(t, func() error { return cmdPreview(args) }); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, agentBindingPath))
	if !reflect.DeepEqual(methods, []string{"GET", "PUT"}) || string(before) != string(after) {
		t.Fatal("candidate continuation mutated local state or used wrong methods")
	}
}
