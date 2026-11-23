package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestConnectorInspectReportsReadinessAndManagementFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessToken {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/connectors/connector-1":
			writeConnectorProto(t, w, &airlockv1.GetConnectorResponse{Connector: &airlockv1.ConnectorInfo{
				Id: "connector-1", DisplayName: "Zigbee", Kind: "zigbee2mqtt", ContractId: "example.zigbee",
				Readiness: "offline", ReadinessMessage: "connector was not reported by host", HostId: "host-1",
			}})
		case "/api/v1/hosts/host-1":
			writeConnectorProto(t, w, &airlockv1.GetHostResponse{ManagementJobs: []*airlockv1.HostManagementJobInfo{{
				Id: fixtureJobID, ConnectorId: "connector-1", Kind: "connector_install", Status: "failed", ErrorMessage: "connect to Mosquitto: connection refused",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}

	output := captureCommandStdout(t, func() error {
		return cmdConnectors([]string{"inspect", "connector-1", "--url", server.URL})
	})
	for _, want := range []string{
		"Readiness: offline",
		"Readiness detail: connector was not reported by host",
		"Latest management job: " + fixtureJobID + " — connector_install (failed)",
		"Management error: connect to Mosquitto: connection refused",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestConnectorInspectManagementDiagnosticsProjection(t *testing.T) {
	for _, hostAllowed := range []bool{false, true} {
		for _, machine := range []bool{false, true} {
			t.Run(fmt.Sprintf("hostAllowed=%t/json=%t", hostAllowed, machine), func(t *testing.T) {
				t.Chdir(t.TempDir())
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("AIRLOCK_DEVELOPMENT_SESSION", "")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/connectors/" + fixtureJobID:
						writeConnectorProto(t, w, &airlockv1.GetConnectorResponse{Connector: &airlockv1.ConnectorInfo{Id: fixtureJobID, HostId: fixtureHostID, ArtifactVersion: "1.2.3"}})
					case "/api/v1/hosts/" + fixtureHostID:
						if !hostAllowed {
							http.Error(w, "private denial body", 403)
							return
						}
						writeConnectorProto(t, w, &airlockv1.GetHostResponse{ManagementJobs: []*airlockv1.HostManagementJobInfo{{Id: "unrelated-private-job", ConnectorId: "other-connector"}, {Id: fixtureJobID, ConnectorId: fixtureJobID, Kind: "connector_update", Status: "failed", ErrorMessage: "update failed"}}})
					default:
						t.Errorf("unexpected route %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				if err := saveLoginCredentials(server.URL, "operator@example.test", testAccessToken, ""); err != nil {
					t.Fatal(err)
				}
				args := []string{"connectors", "inspect", fixtureJobID, "--url", server.URL}
				if machine {
					args = append(args, "--json")
				}
				out, err := invokeFixture(t, args...)
				if err != nil || strings.Contains(out, "unrelated-private-job") || strings.Contains(out, "private denial body") {
					t.Fatalf("projection: %v %s", err, out)
				}
				if hostAllowed && !strings.Contains(out, "update failed") {
					t.Fatalf("missing management state: %s", out)
				}
				if !hostAllowed && !strings.Contains(strings.ToLower(out), "unavailable") {
					t.Fatalf("silent diagnostics failure: %s", out)
				}
				if machine {
					decodeResult(t, out)
				}
			})
		}
	}
}

func writeConnectorProto(t *testing.T, w http.ResponseWriter, message proto.Message) {
	t.Helper()
	body, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(body)
}
