package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestConnectorManagementSeparationJourneys(t *testing.T) {
	var receipts []reviewReceipt
	for _, operation := range []string{"install", "update"} {
		for _, state := range []string{"approval", "accepted", "ready", "failed older installation", "older server recovery", "diagnostics"} {
			for _, machine := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/json=%t", operation, state, machine), func(t *testing.T) {
					t.Chdir(t.TempDir())
					t.Setenv("XDG_CONFIG_HOME", t.TempDir())
					t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
					t.Setenv("SOL_COMMAND_CONTROL_SOCKET", "")
					posts := 0
					digest := strings.Repeat("a", 64)
					revision := int64(8)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
							t.Error("lost developer credential")
						}
						switch {
						case strings.HasSuffix(r.URL.Path, "/artifacts"):
							writeConnectorProto(t, w, &airlockv1.ListConnectorArtifactsResponse{ArtifactSets: []*airlockv1.ConnectorArtifactSetInfo{{Files: []*airlockv1.ConnectorArtifactFileInfo{{Id: fixtureJobID, Sha256: digest}}}}})
						case r.URL.Path == "/api/v1/connectors/"+fixtureJobID:
							writeConnectorProto(t, w, &airlockv1.GetConnectorResponse{Connector: &airlockv1.ConnectorInfo{Id: fixtureJobID, HostId: fixtureHostID, Readiness: "ready", ArtifactVersion: "1.2.3", ArtifactDigest: digest, InventoryRevision: 8, ActiveObservationState: "known", ObservedActiveDigest: digest}})
						case r.URL.Path == "/api/v1/hosts/"+fixtureHostID:
							writeConnectorProto(t, w, &airlockv1.GetHostResponse{ManagementJobs: []*airlockv1.HostManagementJobInfo{{Id: fixtureJobID, ConnectorId: fixtureJobID, Kind: "connector_update", Status: "failed", ErrorMessage: "candidate failed; existing installation retained"}}})
						case r.Method == "POST":
							posts++
							if state == "approval" {
								w.Header().Set("Airlock-Operation-Challenge", fixtureJobID)
								http.Error(w, "confirmation required", 428)
								return
							}
							status := "succeeded"
							if state == "accepted" {
								status = "queued"
							}
							if state == "failed older installation" {
								status = "failed"
							}
							writeConnectorProto(t, w, &airlockv1.GetHostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{Id: fixtureJobID, RequestId: fixtureJobID, HostId: fixtureHostID, ConnectorId: fixtureJobID, ArtifactFileId: fixtureJobID, Kind: "connector_" + operation, Status: status, InventoryRevision: &revision, InventoryAcknowledgedAt: timestamppb.Now(), ErrorMessage: map[bool]string{true: "candidate failed; existing installation retained"}[status == "failed"]}})
						case strings.HasPrefix(r.URL.Path, "/api/v1/hosts/"+fixtureHostID+"/management-requests/"):
							http.NotFound(w, r)
						default:
							t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					for key, value := range map[string]string{"AIRLOCK_DEVELOPMENT_SESSION": previewWorkspaceID, "AIRLOCK_API_URL": server.URL, "AIRLOCK_TOKEN": testAccessToken} {
						t.Setenv(key, value)
					}
					args := []string{"connectors", operation, "--host", fixtureHostID, "--agent", testBuildAgentID, "--need", "sensor", "--artifact", fixtureJobID, "--request-id", fixtureJobID}
					if operation == "install" {
						args = append(args, "--name", "Sensor")
					} else {
						args = append(args, "--connector", fixtureJobID)
					}
					if state == "accepted" {
						args = append(args, "--no-wait")
					}
					if state == "older server recovery" {
						args = []string{"hosts", "request", fixtureHostID, fixtureJobID, "--url", server.URL}
					}
					if state == "diagnostics" {
						args = []string{"connectors", "inspect", fixtureJobID, "--url", server.URL}
					}
					if machine {
						args = append(args, "--json")
					}
					receipt := captureReviewCommand(t, operation+" "+state, args)
					receipt.Mutations = posts
					receipts = append(receipts, receipt)
					if os.Getenv("AIR_CLI_COMPARE_BASELINE") != "" {
						return
					}
					wantExit := 0
					if state == "approval" {
						wantExit = 77
					} else if state == "failed older installation" || state == "older server recovery" {
						wantExit = 1
					}
					if receipt.ExitCode != wantExit {
						t.Fatalf("exit: %+v", receipt)
					}
					wantPosts := 1
					if state == "older server recovery" || state == "diagnostics" {
						wantPosts = 0
					}
					if posts != wantPosts {
						t.Fatalf("mutation replay: %+v", receipt)
					}
					if state == "older server recovery" && !strings.Contains(receipt.Stdout+receipt.Stderr, "neither proves the original operation was not admitted") {
						t.Fatalf("unsafe missing-request guidance: %+v", receipt)
					}
					if machine {
						result := decodeResult(t, receipt.Stdout)
						if state == "approval" && (result.Status != "pending" || result.Error.Code != "approval_required" || result.Error.ApprovalID != fixtureJobID || result.Operation.Admission != "blocked" || result.Guidance[0].Kind != "approval") {
							t.Fatalf("approval contract: %+v", receipt)
						}
						if state == "accepted" && (result.Status != "accepted" || result.Operation.Admission != "accepted") {
							t.Fatalf("admission lost: %+v", receipt)
						}
						if state == "failed older installation" && result.Operation.Admission != "accepted" {
							t.Fatalf("failure erased admission: %+v", receipt)
						}
					} else if state == "approval" && !strings.Contains(receipt.Stderr, "developer conversation") {
						t.Fatalf("human approval direction: %+v", receipt)
					}
				})
			}
		}
	}
	if path := os.Getenv("AIR_CLI_MANAGEMENT_RECEIPTS"); path != "" {
		raw, err := json.MarshalIndent(receipts, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectorManagementWaitsForVerifiedInventory(t *testing.T) {
	for _, operation := range []string{"install", "update"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
			connector, artifact, agent := "20000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000003"
			digest := strings.Repeat("a", 64)
			revision := int64(8)
			posts, reads, polls := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
					t.Error("missing normal login")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/artifacts"):
					writeConnectorProto(t, w, &airlockv1.ListConnectorArtifactsResponse{ArtifactSets: []*airlockv1.ConnectorArtifactSetInfo{{Files: []*airlockv1.ConnectorArtifactFileInfo{{Id: artifact, Sha256: digest}}}}})
				case r.Method == http.MethodPost:
					posts++
					body, _ := io.ReadAll(r.Body)
					if operation == "install" {
						var request airlockv1.RequestConnectorInstallRequest
						if err := protojson.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						if r.URL.Path != "/api/v1/hosts/"+fixtureHostID+"/connectors" || request.AgentId != agent || request.ArtifactFileId != artifact || request.NeedSlug != "sensor" || request.DisplayName != "Sensor" {
							t.Errorf("bad install %s %s", r.URL.Path, body)
						}
					} else {
						var request airlockv1.RequestConnectorUpdateRequest
						if err := protojson.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						if r.URL.Path != "/api/v1/connectors/"+connector+"/update" || request.ArtifactFileId != artifact || request.SettingsJson != "" {
							t.Errorf("bad update %s %s", r.URL.Path, body)
						}
					}
					writeConnectorProto(t, w, &airlockv1.HostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, InventoryRevision: &revision, VerificationPending: true, Id: fixtureJobID, HostId: fixtureHostID, ConnectorId: connector, Kind: "connector_" + operation, ArtifactFileId: artifact, Status: "succeeded"}})
				case r.URL.Path == "/api/v1/connectors/"+connector:
					reads++
					state := "known"
					observed := uint64(7)
					if polls >= 2 {
						observed = 8
					}
					writeConnectorProto(t, w, &airlockv1.GetConnectorResponse{Connector: &airlockv1.ConnectorInfo{InventoryRevision: observed, Id: connector, HostId: fixtureHostID, Readiness: "ready", ArtifactDigest: digest, ActiveObservationState: state, ObservedActiveDigest: digest}})
				case r.URL.Path == "/api/v1/host-management-jobs/"+fixtureJobID:
					polls++
					writeConnectorProto(t, w, &airlockv1.HostManagementJobResponse{Job: &airlockv1.HostManagementJobInfo{RequestId: fixtureJobID, InventoryRevision: &revision, InventoryAcknowledgedAt: timestamppb.Now(), Id: fixtureJobID, HostId: fixtureHostID, ConnectorId: connector, Kind: "connector_" + operation, ArtifactFileId: artifact, Status: "succeeded"}})
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
				t.Fatal(err)
			}
			args := []string{operation, "--url", server.URL, "--request-id", fixtureJobID, "--host", fixtureHostID, "--agent", agent, "--need", "sensor", "--artifact", artifact}
			if operation == "install" {
				args = append(args, "--name", "Sensor")
			} else {
				args = append(args, "--connector", connector)
			}
			output := captureCommandStdout(t, func() error { return cmdConnectors(args) })
			if posts != 1 || reads < 2 || polls < 2 || !strings.Contains(output, "Connector ready — verified "+operation) {
				t.Fatalf("posts %d reads %d polls %d output %s", posts, reads, polls, output)
			}
		})
	}
}

func TestConnectorManagementRejectsForeignJob(t *testing.T) {
	job := &airlockv1.HostManagementJobInfo{Id: fixtureJobID, HostId: fixtureJobID, ConnectorId: fixtureJobID, Kind: "connector_install", ArtifactFileId: fixtureJobID, Status: "queued"}
	if err := validateConnectorManagementJob(job, fixtureHostID, "", "connector_install", fixtureJobID); err == nil {
		t.Fatal("foreign host accepted")
	}
}

func TestConnectorUpdateCrossHostPreflightDeniesBeforeEffects(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			t.Error("cross-host update submitted")
			http.Error(w, "denied", 403)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			writeConnectorProto(t, w, &airlockv1.ListConnectorArtifactsResponse{ArtifactSets: []*airlockv1.ConnectorArtifactSetInfo{{Files: []*airlockv1.ConnectorArtifactFileInfo{{Id: fixtureJobID, Sha256: strings.Repeat("a", 64)}}}}})
			return
		}
		writeConnectorProto(t, w, &airlockv1.GetConnectorResponse{Connector: &airlockv1.ConnectorInfo{Id: fixtureJobID, HostId: fixtureJobID}})
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	err := cmdConnectors([]string{"update", "--url", server.URL, "--host", fixtureHostID, "--connector", fixtureJobID, "--agent", fixtureJobID, "--artifact", fixtureJobID, "--need", "sensor"})
	if err == nil || posts != 0 {
		t.Fatalf("error %v posts %d", err, posts)
	}
}
