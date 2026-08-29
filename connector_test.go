package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector"
	"github.com/airlockrun/agentsdk/connector/protocol"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

type connectorCallInput struct {
	Value string `json:"value"`
}

func TestConnectorDirectoryRoutesAreExact(t *testing.T) {
	contract := connector.DefineContract("io.airlockrun.directory_routes")
	directory := connector.DefineDirectory(contract, "files", connector.DirectoryOptions{Revision: 1, Read: true, Write: true, List: true})
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/list"):
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case strings.HasSuffix(r.URL.Path, "/read"):
			_, _ = w.Write([]byte(`{"entry":{},"data":""}`))
		case strings.HasSuffix(r.URL.Path, "/delete"), strings.HasSuffix(r.URL.Path, "/move"):
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	agent := New(Config{Description: "directory routes"})
	handle := agent.RegisterConnector(&Connector{Slug: "host", Description: "Host files.", Requires: connector.Require(directory)})
	agent.phase, agent.client = agentRunning, newAirlockClient(server.URL, "token", server.Client())
	ctx := context.Background()
	_, _ = handle.List(ctx, directory, protocol.DirectoryListRequest{Path: "a b", Cursor: "last", Limit: 2})
	_, _ = handle.Stat(ctx, directory, "a b.txt")
	_, _ = handle.Read(ctx, directory, protocol.DirectoryReadRequest{Path: "a b.txt", Offset: 1, Length: 2})
	_, _ = handle.Write(ctx, directory, protocol.DirectoryWriteRequest{Path: "x", Data: []byte("x")})
	_ = handle.Delete(ctx, directory, "x")
	_ = handle.Move(ctx, directory, protocol.DirectoryMoveRequest{From: "x", To: "y"})
	want := []string{
		"GET /api/agent/connectors/host/directories/files/list?cursor=last&limit=2&path=a+b",
		"GET /api/agent/connectors/host/directories/files/stat?path=a+b.txt",
		"GET /api/agent/connectors/host/directories/files/read?length=2&offset=1&path=a+b.txt",
		"POST /api/agent/connectors/host/directories/files/write",
		"DELETE /api/agent/connectors/host/directories/files/delete",
		"POST /api/agent/connectors/host/directories/files/move",
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

type connectorCallOutput struct {
	Result string `json:"result"`
}

func TestRegisterAndCallConnector(t *testing.T) {
	contract := connector.DefineContract("io.airlockrun.agent_test")
	command := connector.DefineCommand[connectorCallInput, connectorCallOutput](contract, "execute", connector.CommandOptions{Revision: 1})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/connectors/worker/commands/execute" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "job-1", "status": "success", "output": map[string]string{"result": "ok"}})
	}))
	t.Cleanup(server.Close)
	agent := New(Config{Description: "test"})
	handle := agent.RegisterConnector(&Connector{Slug: "worker", Description: "Runs work.", Requires: connector.Require(command)})
	agent.phase = agentRunning
	agent.client = newAirlockClient(server.URL, "token", server.Client())
	output, err := CallConnector(context.Background(), handle, command, connectorCallInput{Value: "input"})
	if err != nil {
		t.Fatal(err)
	}
	if output.Result != "ok" {
		t.Fatalf("output = %+v", output)
	}
	manifest := agent.Manifest()
	if len(manifest.Connectors) != 1 || manifest.Connectors[0].Requirement.ContractID != contract.ID() {
		t.Fatalf("manifest connectors = %+v", manifest.Connectors)
	}
}

func TestStartConnectorJobSendsExactTypedRequestAndReusesHandle(t *testing.T) {
	contract := connector.DefineContract("io.airlockrun.agent_job_test")
	command := connector.DefineCommand[connectorCallInput, connectorCallOutput](contract, "execute", connector.CommandOptions{Revision: 3, Mode: protocol.CommandModeJob})
	requestID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/agent/connectors/worker/jobs/execute":
			var request protocol.CommandCallRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			descriptor := command.Descriptor()
			if request.RequestID != requestID.String() || request.Revision != descriptor.Revision || request.Mode != descriptor.Mode || request.InputSchemaHash != descriptor.InputSchemaHash || request.OutputSchemaHash != descriptor.OutputSchemaHash {
				t.Errorf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(protocol.CommandCallResponse{JobID: "job-1", Status: "queued"})
		case "GET /api/agent/connectors/worker/jobs/job-1":
			_ = json.NewEncoder(w).Encode(wire.ConnectorJobInfo{JobID: "job-1", Status: "success", Output: json.RawMessage(`{"result":"ok"}`), History: []wire.ConnectorJobProgress{}})
		case "DELETE /api/agent/connectors/worker/jobs/job-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	agent := New(Config{Description: "test"})
	handle := agent.RegisterConnector(&Connector{Slug: "worker", Description: "Runs work.", Requires: connector.Require(command)})
	agent.phase, agent.client = agentRunning, newAirlockClient(server.URL, "token", server.Client())
	job, err := StartConnectorJob(context.Background(), handle, requestID, command, connectorCallInput{Value: "input"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := job.Wait(context.Background())
	if err != nil || output.Output.Result != "ok" || len(output.Info.History) != 0 {
		t.Fatalf("Wait() = %+v, %v", output, err)
	}
	if err := job.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartConnectorOrchestrationReturnsDurableHandle(t *testing.T) {
	contract := connector.DefineContract("io.airlockrun.agent_orchestration_test")
	command := connector.DefineCommand[connectorCallInput, connectorCallOutput](contract, "execute", connector.CommandOptions{Revision: 2, Mode: protocol.CommandModeJob})
	requestID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/agent/connectors/fleet/orchestrations":
			var request wire.ConnectorOrchestrationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			descriptor := command.Descriptor()
			if request.CommandName != command.Name() || request.Request.RequestID != requestID.String() || request.Request.CanaryCount != 1 || request.Request.Command.Revision != descriptor.Revision || request.Request.Command.Mode != descriptor.Mode || request.Request.Command.OutputSchemaHash != descriptor.OutputSchemaHash {
				t.Errorf("request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(wire.ConnectorOrchestrationInfo{ID: "orchestration-1", Status: "running", Targets: []wire.ConnectorJobInfo{}})
		case "GET /api/agent/connectors/fleet/orchestrations/orchestration-1":
			_ = json.NewEncoder(w).Encode(wire.ConnectorOrchestrationInfo{ID: "orchestration-1", Status: "success", CanaryPhase: "remainder", Targets: []wire.ConnectorJobInfo{}})
		case "DELETE /api/agent/connectors/fleet/orchestrations/orchestration-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	agent := New(Config{Description: "test"})
	handle := agent.RegisterConnector(&Connector{Slug: "fleet", Description: "Runs fleet work.", Multiple: true, Requires: connector.Require(command)})
	agent.phase, agent.client = agentRunning, newAirlockClient(server.URL, "token", server.Client())
	deadline := time.Now().Add(time.Minute)
	orchestration, err := StartConnectorOrchestration(context.Background(), handle, command, protocol.OrchestrationRequest{
		RequestID: requestID.String(), Strategy: protocol.StrategyCanary, OfflinePolicy: protocol.OfflineWait,
		MaxConcurrency: 2, BatchSize: 1, CanaryCount: 1, Deadline: &deadline,
	}, connectorCallInput{Value: "input"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := orchestration.Wait(context.Background())
	if err != nil || info.Status != "success" {
		t.Fatalf("Wait() = %+v, %v", info, err)
	}
	if err := orchestration.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConnectorOrchestrationValidatesInputAndReturnsFailure(t *testing.T) {
	contract := connector.DefineContract("io.airlockrun.agent_orchestration_failure")
	command := connector.DefineCommand[connectorCallInput, connectorCallOutput](contract, "execute", connector.CommandOptions{Revision: 1, Mode: protocol.CommandModeJob})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(wire.ConnectorOrchestrationInfo{ID: "orchestration-1", Status: "error", Targets: []wire.ConnectorJobInfo{{JobID: "job-1", Error: "target failed"}}})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(wire.ConnectorOrchestrationInfo{ID: "orchestration-1", Status: "error", Targets: []wire.ConnectorJobInfo{{JobID: "job-1", Error: "target failed"}}})
		}
	}))
	defer server.Close()
	agent := New(Config{Description: "test"})
	handle := agent.RegisterConnector(&Connector{Slug: "fleet", Description: "Runs fleet work.", Multiple: true, Requires: connector.Require(command)})
	agent.phase, agent.client = agentRunning, newAirlockClient(server.URL, "token", server.Client())
	request := protocol.OrchestrationRequest{RequestID: uuid.Nil.String(), Strategy: protocol.StrategyParallel, OfflinePolicy: protocol.OfflineFail}
	if _, err := StartConnectorOrchestration(context.Background(), handle, command, request, connectorCallInput{}); err == nil {
		t.Fatal("nil orchestration UUID was accepted")
	}
	request.RequestID = uuid.NewString()
	if _, err := StartConnectorOrchestration(context.Background(), handle, command, request, connectorCallInput{Value: strings.Repeat("x", protocol.MaxJobPayloadBytes)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized orchestration error = %v", err)
	}
	orchestration, err := StartConnectorOrchestration(context.Background(), handle, command, request, connectorCallInput{})
	if err != nil {
		t.Fatal(err)
	}
	if orchestration == nil || orchestration.ID() != "orchestration-1" {
		t.Fatalf("replayed terminal orchestration handle = %#v", orchestration)
	}
	if _, err := orchestration.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "target failed") {
		t.Fatalf("orchestration failure = %v", err)
	}
}

func TestConnectorOrchestrationReplayReturnsHandleForTerminalFailure(t *testing.T) {
	for _, status := range []string{"error", "canceled"} {
		t.Run(status, func(t *testing.T) {
			contract := connector.DefineContract("io.airlockrun.agent_orchestration_replay")
			command := connector.DefineCommand[connectorCallInput, connectorCallOutput](contract, "execute", connector.CommandOptions{Revision: 1, Mode: protocol.CommandModeJob})
			info := wire.ConnectorOrchestrationInfo{ID: "orchestration-1", Status: status}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(info)
			}))
			defer server.Close()
			agent := New(Config{Description: "test"})
			handle := agent.RegisterConnector(&Connector{Slug: "fleet", Description: "Runs fleet work.", Multiple: true, Requires: connector.Require(command)})
			agent.phase, agent.client = agentRunning, newAirlockClient(server.URL, "token", server.Client())
			request := protocol.OrchestrationRequest{RequestID: uuid.NewString(), Strategy: protocol.StrategyParallel, OfflinePolicy: protocol.OfflineFail}
			orchestration, err := StartConnectorOrchestration(context.Background(), handle, command, request, connectorCallInput{})
			if err != nil || orchestration == nil || orchestration.ID() != info.ID {
				t.Fatalf("StartConnectorOrchestration() = %#v, %v", orchestration, err)
			}
			if _, err := orchestration.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), status) {
				t.Fatalf("Wait() error = %v", err)
			}
		})
	}
}
