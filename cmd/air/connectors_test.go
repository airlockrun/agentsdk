package main

import (
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
		if got := r.Header.Get("Authorization"); got != "Bearer user-token" {
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
				ConnectorId: "connector-1", Kind: "connector_install", Status: "failed", ErrorMessage: "connect to Mosquitto: connection refused",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := saveLoginCredentials(server.URL, "operator@example.com", "user-token", ""); err != nil {
		t.Fatal(err)
	}

	output := captureCommandStdout(t, func() error {
		return cmdConnectors([]string{"inspect", "connector-1", "--url", server.URL})
	})
	for _, want := range []string{
		"Readiness: offline",
		"Readiness detail: connector was not reported by host",
		"Latest management job: connector_install (failed)",
		"Management error: connect to Mosquitto: connection refused",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
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
