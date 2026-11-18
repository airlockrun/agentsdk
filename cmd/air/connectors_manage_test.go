package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
			if posts != 1 || reads < 2 || polls < 2 || !strings.Contains(output, "Verified connector "+connector) {
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
