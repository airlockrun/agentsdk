package remote

import (
	"context"
	"fmt"
	"github.com/airlockrun/agentsdk/wire"
	"google.golang.org/protobuf/types/known/structpb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScopedRunnerNeverDiscoversOrUsesOrdinaryLogin(t *testing.T) {
	testID := "66666666-6666-6666-6666-666666666666"
	root := "/api/test-runs/" + testID
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer runner-capability" || r.Header.Get(wire.ExpectedSourceRevisionHeader) != revisionID {
			t.Error("incorrect runner capability/pin")
		}
		switch r.URL.Path {
		case root + "/http/":
			fmt.Fprint(w, "real app response")
		case root + "/tools/report/invoke":
			fmt.Fprintf(w, `{"runId":%q,"resultJson":"{}"}`, runID)
		default:
			t.Errorf("runner escaped scoped root: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewTestClient(TestConfig{APIRoot: server.URL + root, Token: "runner-capability", AppID: appID, DeploymentID: runtimeID, Revision: revisionID, RuntimeGeneration: 2, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Request(t.Context(), "GET", "/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	input, _ := structpb.NewValue(map[string]any{})
	if _, err := client.InvokeTool(t.Context(), "report", input); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("extra discovery requests: %v", paths)
	}
	if _, err := client.api.Get(t.Context()); err == nil {
		t.Fatal("runner discovered ordinary deployment")
	}
}
func TestRunnerEnvironmentRequiresAllCoordinates(t *testing.T) {
	for _, name := range []string{"AIRLOCK_TEST_API_URL", "AIRLOCK_TEST_TOKEN", "AIRLOCK_TEST_APP_ID", "AIRLOCK_TEST_DEPLOYMENT_ID", "AIRLOCK_TEST_SOURCE_REVISION", "AIRLOCK_TEST_RUNTIME_GENERATION"} {
		t.Setenv(name, "")
	}
	if _, err := TestClientFromEnvironment(); err == nil {
		t.Fatal("missing runner config silently selected a local environment")
	}
	_, err := NewTestClient(TestConfig{APIRoot: "https://airlock.example/api/v1/agents/" + appID, Token: "user-token", AppID: appID, DeploymentID: runtimeID, Revision: revisionID, RuntimeGeneration: 1, HTTPClient: &http.Client{}})
	if err == nil || !strings.Contains(err.Error(), "exact runner root") {
		t.Fatal(err)
	}
}

func TestPinnedOperatorSendsAtomicRevisionHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(wire.ExpectedSourceRevisionHeader) != revisionID {
			t.Error("missing atomic revision header")
		}
		if strings.HasSuffix(r.URL.Path, "/http/") {
			http.Error(w, "revision changed", 409)
			return
		}
		deploymentResponse(w)
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Request(context.Background(), "GET", "/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatalf("stale revision executed: HTTP %d", response.StatusCode)
	}
}
