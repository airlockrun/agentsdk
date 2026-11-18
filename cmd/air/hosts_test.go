package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const fixtureHostID = "10000000-0000-4000-8000-000000000001"
const fixtureJobID = "10000000-0000-4000-8000-000000000002"

func TestHostsExecNormalLoginExactRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/hosts/"+fixtureHostID+"/shell" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		posts++
		if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Error("ordinary login not used")
		}
		var request airlockv1.RequestHostShellRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := protojson.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		if request.Command != "/bin/printf" || len(request.Arguments) != 2 || request.Arguments[1] != "--host=not-a-flag" || request.Environment["VALUE"] != "a=b" || request.WorkingDirectory != "/work" || request.TimeoutSeconds != 7 {
			t.Errorf("request = %v", &request)
		}
		writeConnectorProto(t, w, &airlockv1.HostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: request.RequestId, Id: fixtureJobID, HostId: fixtureHostID, Kind: "shell", Status: "succeeded", OutputJson: `{"stdout":"b2s=","stderr":"","exitCode":0}`}})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	output := captureCommandStdout(t, func() error {
		return run([]string{"hosts", "exec", fixtureHostID, "--url", server.URL, "--cwd", "/work", "--env", "VALUE=a=b", "--timeout", "7s", "--", "/bin/printf", "%s", "--host=not-a-flag"})
	})
	if output != "ok" || posts != 1 {
		t.Fatalf("output %q posts %d", output, posts)
	}
}

func TestHostsExecDenialNeverResubmits(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusPreconditionRequired} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "workspace")
			t.Setenv("AIRLOCK_TOKEN", "developer-token")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"denied"}`))
			}))
			defer server.Close()
			t.Setenv("AIRLOCK_API_URL", server.URL)
			err := run([]string{"hosts", "exec", fixtureHostID, "--", "true"})
			if err == nil || calls != 1 {
				t.Fatalf("error %v calls %d", err, calls)
			}
		})
	}
}

func TestHostsExecJSONReportsRemoteExit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeConnectorProto(t, w, &airlockv1.HostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, Id: fixtureJobID, HostId: fixtureHostID, Kind: "shell", Status: "succeeded", OutputJson: `{"exitCode":23}`}})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	var commandErr error
	output := captureCommandStdout(t, func() error {
		commandErr = run([]string{"hosts", "exec", fixtureHostID, "--url", server.URL, "--request-id", fixtureJobID, "--json", "--", "false"})
		return nil
	})
	var exit *hostCommandExitError
	if !errors.As(commandErr, &exit) || exit.code != 23 || !strings.Contains(output, fixtureJobID) {
		t.Fatalf("error %v output %s", commandErr, output)
	}
}

func TestHostsRejectsInvalidTargetsAndOptions(t *testing.T) {
	for _, args := range [][]string{
		{"exec", "host-name", "--", "true"},
		{"exec", fixtureHostID, "true"},
		{"exec", fixtureHostID, "--timeout", "1ms", "--", "true"},
		{"exec", fixtureHostID, "--env", "X=1", "--env", "X=2", "--", "true"},
		{"job", fixtureJobID, "--cwd", "/work"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := cmdHosts(args); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	job := &airlockv1.HostManagementJobInfo{Id: fixtureJobID, HostId: fixtureJobID, Kind: "shell", Status: "queued"}
	if err := validateHostJob(job, "exec", fixtureHostID); err == nil {
		t.Fatal("cross-host response accepted")
	}
}

func TestHostsExecPollsExistingJobWithoutResubmission(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	posts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		job := &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, Id: fixtureJobID, HostId: fixtureHostID, Kind: "shell", Status: "queued"}
		if r.Method == http.MethodPost {
			posts++
		} else {
			gets++
			if r.URL.Path != "/api/v1/host-management-jobs/"+fixtureJobID {
				t.Errorf("path %s", r.URL.Path)
			}
			job.Status = "succeeded"
			job.OutputJson = `{"exitCode":0,"stdout":"ZG9uZQ=="}`
		}
		writeConnectorProto(t, w, &airlockv1.GetHostManagementJobResponse{Job: job})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	output := captureCommandStdout(t, func() error {
		return cmdHosts([]string{"exec", fixtureHostID, "--url", server.URL, "--request-id", fixtureJobID, "--", "true"})
	})
	if posts != 1 || gets != 1 || output != "done" {
		t.Fatalf("posts %d gets %d output %s", posts, gets, output)
	}
}

func TestHostsCancelUsesCanonicalAuthorizedEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/host-management-jobs/"+fixtureJobID || r.Header.Get("Authorization") != "Bearer "+testAccessToken {
			t.Errorf("unexpected cancel %s %s", r.Method, r.URL.Path)
		}
		deletes++
		writeConnectorProto(t, w, &airlockv1.GetHostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{CancelRequestedAt: timestamppb.Now(), Id: fixtureJobID, HostId: fixtureHostID, Kind: "shell", Status: "running"}})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	output := captureCommandStdout(t, func() error { return cmdHosts([]string{"cancel", fixtureJobID, "--url", server.URL}) })
	if deletes != 1 || !strings.Contains(output, "Cancellation requested") || !strings.Contains(output, "Earlier effects may exist") {
		t.Fatal(output, deletes)
	}
}

func TestHostsRequestRecoversOnlyExactAdmission(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/hosts/"+fixtureHostID+"/management-requests/"+fixtureJobID {
			t.Errorf("recovery path %s %s", r.Method, r.URL.Path)
		}
		writeConnectorProto(t, w, &airlockv1.GetHostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, Id: fixtureJobID, HostId: fixtureHostID, Kind: "connector_update", Status: "succeeded", VerificationPending: true}})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	output := captureCommandStdout(t, func() error { return cmdHosts([]string{"request", fixtureHostID, fixtureJobID, "--url", server.URL}) })
	if calls != 1 || !strings.Contains(output, "inventory verification pending") {
		t.Fatal("succeeded-unverified request not preserved", calls, output)
	}
}
