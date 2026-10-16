package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSuiteUsesExactRevisionAndPreservesFailure(t *testing.T) {
	var posts, deletes int
	base := "/api/v1/agents/" + appID + "/deployments/p-random"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base {
			deploymentResponse(w)
			return
		}
		if r.Method == "POST" {
			posts++
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["sourceRevisionId"] != revisionID || input["suite"] != "integration" {
				t.Errorf("input=%v", input)
			}
		}
		if r.Method == "DELETE" {
			deletes++
		}
		fmt.Fprintf(w, `{"test":{"id":%q,"appId":%q,"deploymentId":%q,"sourceRevisionId":%q,"suite":"integration","status":"failed","exitCode":1,"error":"assertion failed"}}`, runID, appID, runtimeID, revisionID)
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.RunTests(t.Context(), &TestRequest{SourceRevisionId: revisionID, Suite: "integration", TimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	info, err = client.WaitTest(t.Context(), info.Id, time.Millisecond)
	if err != nil || info.Status != "failed" || info.ExitCode != 1 || posts != 1 || deletes != 0 {
		t.Fatalf("result=%v error=%v", info, err)
	}
}
func TestSuiteWaitTimeoutDoesNotCancelOrChangeTargets(t *testing.T) {
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/deployments/p-random/tests/") {
			t.Error("wrong target")
		}
		if r.Method == "DELETE" {
			deletes++
		}
		fmt.Fprintf(w, `{"test":{"id":%q,"appId":%q,"deploymentId":%q,"sourceRevisionId":%q,"status":"running"}}`, runID, appID, runtimeID, revisionID)
	}))
	defer server.Close()
	client, err := New(configFor(server))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.WaitTest(ctx, runID, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if deletes != 0 {
		t.Fatal("observation cancelled remote suite")
	}
}
