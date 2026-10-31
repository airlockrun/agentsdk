package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPreviewSelectionIsolationCopyAndDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	created := 0
	targets := map[string]string{}
	apps := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("unexpected credentials")
		}
		if r.Header.Get("X-Airlock-Preview-Workspace") != "" || r.Header.Get("X-Airlock-Remote") != "" {
			t.Error("synthetic identity header")
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
		app, suffix, _ := strings.Cut(path, "/")
		if suffix == "" {
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, app)
			return
		}
		selector := strings.TrimPrefix(suffix, "deployments/")
		if selector == "preview" && r.Method == http.MethodPost {
			created++
			selector = fmt.Sprintf("00000000-0000-4000-8000-%012d", created)
			targets[selector] = "p-" + strings.ReplaceAll(selector, "-", "")
			apps[selector] = app
		}
		if selector == "production" || selector == "staging" {
			fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"name":%q,"kind":%q}}`, app, app, app, selector, selector)
			return
		}
		id := selector
		for candidate, name := range targets {
			if name == selector {
				id = candidate
			}
		}
		if targets[id] == "" || apps[id] != app {
			http.Error(w, "target access denied", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"deployment":{"id":%q,"runtimeAgentId":%q,"appId":%q,"name":%q,"kind":"personal_preview","ownerUserId":%q}}`, id, id, app, targets[id], previewOwnerID)
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	makeCheckout := func() string {
		dir := t.TempDir()
		b := agentBinding{}
		for _, remote := range []string{"corp", "stage"} {
			b.putRemote(remote, agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID})
		}
		if err := writeAgentBinding(dir, b); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a, b := makeCheckout(), makeCheckout()
	resolve := func(dir, remote, app, target string, ensure bool) selectedDeployment {
		s, err := resolveDeployment(t.Context(), targetFlags{dir: dir, remote: remote, agent: app, target: target}, ensure)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := resolveDeployment(t.Context(), targetFlags{dir: a, target: "preview"}, false); err == nil {
		t.Fatal("missing preview did not fail")
	}
	first := resolve(a, "corp", "", "preview", true)
	second := resolve(b, "corp", "", "preview", true)
	otherRemote := resolve(a, "stage", "", "preview", true)
	otherApp := resolve(a, "corp", previewTaskID, "preview", true)
	if created != 4 || first.info.Id == second.info.Id || first.info.Id == otherRemote.info.Id || first.info.Id == otherApp.info.Id {
		t.Fatal("independent selections collided")
	}
	if resolve(a, "corp", "", "preview", true).info.Id != first.info.Id || created != 4 {
		t.Fatal("saved preview was recreated")
	}
	if resolve(a, "corp", "", "", false).info.Name != "production" || resolve(a, "corp", "", "staging", false).info.Name != "staging" {
		t.Fatal("preview changed default or named selection")
	}
	output, err := captureCommandStdoutResult(t, func() error { return cmdTargetStatus([]string{"--dir", a, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Remote     string `json:"remote"`
		Target     string `json:"target"`
		Deployment struct {
			ID string `json:"id"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(output), &metadata); err != nil || metadata.Remote != "corp" || metadata.Target != "production" || metadata.Deployment.ID != testBuildAgentID || strings.Contains(output, testAccessToken) {
		t.Fatalf("resolved JSON metadata: %s %v", output, err)
	}
	if err := first.saveSource(targetSourceBinding{Name: first.info.Name, SourceETag: "first-etag"}); err != nil {
		t.Fatal(err)
	}
	// Move/copy the exact reference and source cache without checkout identity.
	body, err := os.ReadFile(filepath.Join(a, agentBindingPath))
	if err != nil {
		t.Fatal(err)
	}
	copyRoot := t.TempDir()
	mustWrite(t, filepath.Join(copyRoot, agentBindingPath), string(body))
	copied := resolve(copyRoot, "corp", "", "preview", true)
	if copied.info.Id != first.info.Id || copied.app.Targets[first.info.Id].SourceETag != "first-etag" || created != 4 {
		t.Fatal("copied reference/cache changed")
	}
	before, _, _ := loadAgentBinding(b)
	if _, err := captureCommandStdoutResult(t, func() error { return cmdPreviewSelect([]string{first.info.Name, "--dir", b}) }); err != nil {
		t.Fatal(err)
	}
	if resolve(b, "corp", "", "preview", false).info.Id != first.info.Id {
		t.Fatal("explicit selection not saved")
	}
	if err := second.saveSource(targetSourceBinding{Name: second.info.Name, SourceETag: "second-etag"}); err != nil {
		t.Fatal(err)
	}
	if resolve(b, "corp", "", "preview", false).info.Id != first.info.Id {
		t.Fatal("source cache update changed the selected preview")
	}
	after, _, _ := loadAgentBinding(b)
	if after.DefaultRemote != before.DefaultRemote || after.Remotes["corp"].AgentID != testBuildAgentID {
		t.Fatal("selection changed defaults")
	}
	if err := cmdPreviewSelect([]string{"p-denied", "--dir", b}); err == nil {
		t.Fatal("denied target selected")
	}
	denied, _, _ := loadAgentBinding(b)
	if !reflect.DeepEqual(after, denied) {
		t.Fatal("denied selection changed configuration")
	}
	stored, _, err := loadAgentBinding(a)
	if err != nil || len(stored.Remotes["corp"].Previews) != 2 || len(stored.Remotes["stage"].Previews) != 1 {
		t.Fatalf("app/remote isolation: %+v %v", stored, err)
	}
	for _, forbidden := range []string{"context_id", "context_root", "workspace_id", a} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("config contains %q", forbidden)
		}
	}
}

func TestContextCommandNotExposed(t *testing.T) {
	for _, action := range []string{"new", "status", "delete"} {
		if err := run([]string{"context", action}); err == nil {
			t.Fatal("context command exposed")
		}
	}
}

func TestOrdinaryCommandsResolveProductionWithoutTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	resolved := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"app"}}`, testBuildAgentID)
		case "/api/v1/agents/" + testBuildAgentID + "/deployments/production":
			resolved++
			http.Error(w, "selection observed", http.StatusTeapot)
		default:
			t.Errorf("unexpected selection %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "user@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module app\n")
	b := agentBinding{}
	b.putRemote("corp", agentRemoteBinding{AirlockURL: server.URL, AgentID: testBuildAgentID, Previews: map[string]previewReference{testBuildAgentID: {DeploymentID: previewRuntimeID, Name: previewName}}})
	if err := writeAgentBinding(dir, b); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"deploy", dir, "-m", "Production"}, {"deploy", "list", dir}, {"deploy", "status", dir},
		{"status", "--dir", dir}, {"start", "--dir", dir}, {"stop", "--dir", dir}, {"suspend", "--dir", dir},
		{"request", "GET", "/", "--dir", dir}, {"tools", "list", "--dir", dir},
		{"agent", "start", "task", "--dir", dir}, {"logs", previewTaskID, "--dir", dir},
		{"resources", "bindings", "--dir", dir}, {"resources", "bind", "production/crm", "--resource", "/" + previewTaskID, "--dir", dir},
		{"resources", "unbind", "production/crm", "--dir", dir}, {"pull", dir},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			before := resolved
			if err := run(args); err == nil || !hasHTTPStatus(err, http.StatusTeapot) || resolved != before+1 {
				t.Fatalf("command did not resolve production: %v count=%d", err, resolved-before)
			}
		})
	}
}
