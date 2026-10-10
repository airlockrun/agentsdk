package localruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

func testHost(t *testing.T, model stream.Model) (*Host, *State, *httptest.Server, wire.AgentDefinition) {
	t.Helper()
	appID := uuid.NewString()
	state, err := OpenState(t.TempDir(), appID)
	if err != nil {
		t.Fatal(err)
	}
	storage := testFileStorage(t)
	executor, err := Executor(ExecutorConfig{Limits: jsexec.DefaultLimits(), OpenTransport: func(context.Context) (io.ReadWriteCloser, error) {
		t.Error("completion must not open JavaScript")
		return nil, io.ErrClosedPipe
	}})
	if err != nil {
		t.Fatal(err)
	}
	def := wire.AgentDefinition{Slug: "task", Description: "Fixture", Instructions: "Complete fixture", ModelSlot: "reasoning", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Budget: wire.AgentBudget{Steps: 3}, MaxAttempts: 3, MaxConcurrency: 1}
	def.ContractHash, err = wire.AgentDefinitionHash(def)
	if err != nil {
		t.Fatal(err)
	}
	manifest := wire.AgentManifest{RuntimeProtocol: wire.AppRuntimeProtocol, Directories: []wire.DirectoryDef{{Path: "tmp"}}, ModelSlots: []wire.ModelSlotDef{{Slug: "reasoning", Capability: "text"}}, AgentDefinitions: []wire.AgentDefinition{def}}
	host, err := New(Config{AppID: appID, Token: "host-secret", AppURL: "http://127.0.0.1:1", RouteURL: "http://fixture.local", State: state, Storage: storage, Models: map[string]ModelBinding{"reasoning": {Model: model, Limits: session.ModelLimits{Context: 100000, Output: 4096}}}, Client: http.DefaultClient, ExecutorFactory: executor})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host.Handler())
	t.Cleanup(server.Close)
	if status, _ := hostRequest(t, server, "PUT", "/api/agent/sync", manifest, nil); status != 200 {
		t.Fatalf("sync status %d", status)
	}
	return host, state, server, def
}

func hostRequest(t *testing.T, server *httptest.Server, method, path string, input any, headers http.Header) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Authorization", "Bearer host-secret")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func completeModel(t *testing.T) *testutil.MockModel {
	t.Helper()
	model, err := testutil.NewMockModel(testutil.MockConfig{ID: "fixture", Default: &testutil.MockResponse{ToolCalls: []stream.ToolCall{{ID: "done", Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"answer":"42"}}`)}}, Usage: stream.UsageFrom(1, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestDurableTaskLifecycleAndReplay(t *testing.T) {
	model := completeModel(t)
	h, state, server, def := testHost(t, model)
	request := wire.StartAgentRequest{RequestID: uuid.NewString(), ContractHash: def.ContractHash, Input: json.RawMessage(`{"question":"fixture"}`)}
	status, body := hostRequest(t, server, "POST", "/api/agent/agents/task/runs", request, nil)
	if status != 200 {
		t.Fatalf("start %d %s", status, body)
	}
	var started wire.AgentRunResponse
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatal(err)
	}
	if worked, err := h.WorkOnce(t.Context()); err != nil || !worked {
		t.Fatalf("work %v %v", worked, err)
	}
	status, body = hostRequest(t, server, "GET", "/api/agent/agents/task/runs/"+started.Run.ID, nil, nil)
	var completed wire.AgentRunResponse
	json.Unmarshal(body, &completed)
	if status != 200 || completed.Run.Status != "completed" || completed.Run.Steps != 1 || completed.Run.Tokens != 2 || completed.Run.Reply == nil {
		t.Fatalf("completed %d %s", status, body)
	}
	reopened, err := OpenState(state.directory, h.config.AppID)
	if err != nil {
		t.Fatal(err)
	}
	config := h.config
	config.State = reopened
	other, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := other.WorkOnce(t.Context()); err != nil || worked {
		t.Fatalf("completed task replayed: %v %v", worked, err)
	}
	status, body = hostRequest(t, server, "POST", "/api/agent/agents/task/runs", request, nil)
	var replay wire.AgentRunResponse
	json.Unmarshal(body, &replay)
	if status != 200 || replay.Created || replay.Run.ID != started.Run.ID {
		t.Fatalf("replay %d %s", status, body)
	}
	request.Input = json.RawMessage(`{"question":"different"}`)
	if status, _ := hostRequest(t, server, "POST", "/api/agent/agents/task/runs", request, nil); status != 409 {
		t.Fatal("conflicting replay accepted")
	}
	if len(model.Requests()) != 1 {
		t.Fatalf("model calls %d", len(model.Requests()))
	}
}

func TestCheckpointRecoveryNeverRedispatchesUnknownEffect(t *testing.T) {
	model := completeModel(t)
	h, state, server, def := testHost(t, model)
	req := wire.StartAgentRequest{RequestID: uuid.NewString(), ContractHash: def.ContractHash, Input: json.RawMessage(`{}`)}
	_, body := hostRequest(t, server, "POST", "/api/agent/agents/task/runs", req, nil)
	var out wire.AgentRunResponse
	json.Unmarshal(body, &out)
	oldToken := ""
	err := state.update(t.Context(), func(d *stateData) error {
		task := d.Tasks[out.Run.ID]
		oldToken = task.Scope.InvocationToken
		task.Info.Status = "running"
		task.Attempts = 1
		task.Owner = uuid.NewString()
		task.Checkpoint = &agentruntime.Checkpoint{Version: 1, Revision: 1, ContractHash: def.ContractHash, Phase: agentruntime.PhaseDispatch, Calls: []agentruntime.Call{{ID: "unknown", Name: "run_js", Input: json.RawMessage(`{"code":"effect()","description":"effect"}`)}}, CallIDs: []string{"unknown"}}
		task.CheckpointPayload, _ = json.Marshal(task.Checkpoint)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := h.WorkOnce(t.Context()); err != nil || !worked {
		t.Fatalf("recovery %v %v", worked, err)
	}
	var messages []session.Message
	state.update(t.Context(), func(d *stateData) error {
		task := d.Tasks[out.Run.ID]
		if task.Info.Status != "completed" {
			t.Fatalf("task=%+v", task.Info)
		}
		if task.Scope.InvocationToken == oldToken {
			t.Fatal("recovery did not rotate callback receipt")
		}
		messages = task.Messages
		return nil
	})
	raw, _ := json.Marshal(messages)
	if !bytes.Contains(raw, []byte("unknown outcome")) {
		t.Fatalf("missing unknown effect notice: %s", raw)
	}
	headers := http.Header{"X-Airlock-Run-ID": []string{out.Run.ID}, wire.InvocationTokenHeader: []string{oldToken}}
	if status, _ := hostRequest(t, server, "POST", "/api/agent/llm/stream", map[string]any{}, headers); status != 403 {
		t.Fatal("stale receipt accepted")
	}
}

func TestTwoHostsClaimOneTask(t *testing.T) {
	model := completeModel(t)
	h, _, server, def := testHost(t, model)
	hostRequest(t, server, "POST", "/api/agent/agents/task/runs", wire.StartAgentRequest{RequestID: uuid.NewString(), ContractHash: def.ContractHash, Input: json.RawMessage(`{}`)}, nil)
	other, err := New(h.config)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for _, host := range []*Host{h, other} {
		go func() {
			defer wg.Done()
			if _, err := host.WorkOnce(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(model.Requests()) != 1 {
		t.Fatalf("duplicate reasoning calls: %d", len(model.Requests()))
	}
}

func TestWaitingTaskDeadlineSettlesWithoutChildCompletion(t *testing.T) {
	h, state, server, def := testHost(t, completeModel(t))
	def.Budget.TimeoutMS = 1
	def.ContractHash, _ = wire.AgentDefinitionHash(def)
	if err := state.update(t.Context(), func(d *stateData) error { d.Manifest.AgentDefinitions = []wire.AgentDefinition{def}; return nil }); err != nil {
		t.Fatal(err)
	}
	_, body := hostRequest(t, server, "POST", "/api/agent/agents/task/runs", wire.StartAgentRequest{RequestID: uuid.NewString(), ContractHash: def.ContractHash, Input: []byte(`{}`)}, nil)
	var out wire.AgentRunResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if err := state.update(t.Context(), func(d *stateData) error {
		task := d.Tasks[out.Run.ID]
		task.Info.Status = "waiting"
		task.Info.CreatedAt = task.Info.CreatedAt.Add(-time.Second)
		task.WaitRequest = wire.AgentWaitRequest{IDs: []string{uuid.NewString()}, Mode: "all"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WorkOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := state.update(t.Context(), func(d *stateData) error {
		task := d.Tasks[out.Run.ID]
		if task.Info.Status != "budget_exceeded" || task.Info.CompletedAt == nil {
			t.Fatalf("waiting deadline ignored: %+v", task.Info)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptsAndRegisteredStorageBoundary(t *testing.T) {
	h, _, server, _ := testHost(t, completeModel(t))
	for _, headers := range []http.Header{nil, {"X-Airlock-Run-ID": []string{uuid.NewString()}, wire.InvocationTokenHeader: []string{strings.Repeat("ab", 32)}}} {
		if status, _ := hostRequest(t, server, "POST", "/api/agent/llm/stream", map[string]any{}, headers); status != 403 {
			t.Fatal("forged receipt accepted")
		}
	}
	scope, err := h.Admit(t.Context(), appCaller("background"))
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Airlock-Run-ID": []string{scope.RunID}, wire.InvocationTokenHeader: []string{scope.InvocationToken}}
	if status, _ := hostRequest(t, server, "POST", "/api/agent/run/complete", map[string]any{"status": "success"}, headers); status != 204 {
		t.Fatal(status)
	}
	if status, _ := hostRequest(t, server, "POST", "/api/agent/llm/stream", map[string]any{}, headers); status != 403 {
		t.Fatal("closed receipt accepted")
	}
	if status, _ := hostRequest(t, server, "PUT", "/api/agent/storage/unregistered/file", map[string]string{}, nil); status != 403 {
		t.Fatal("unregistered storage accepted")
	}
}

func TestPersistentStorageAcrossReplicas(t *testing.T) {
	dir := t.TempDir()
	first, err := NewFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, err = first.Write(t.Context(), "tmp/file", strings.NewReader("one"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	info, err := second.Stat(t.Context(), "tmp/file")
	if err != nil || info.ContentType != "text/plain" {
		t.Fatalf("metadata=%+v %v", info, err)
	}
	open, err := second.Open(t.Context(), "tmp/file")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	_, err = first.Write(t.Context(), "tmp/file", strings.NewReader("two"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(open)
	if string(raw) != "one" {
		t.Fatal("open immutable version changed")
	}
	if err := second.Copy(t.Context(), "tmp/file", "tmp/copy"); err != nil {
		t.Fatal(err)
	}
	first.Close()
	restarted, err := NewFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	files, err := restarted.List(t.Context(), "tmp", true)
	if err != nil || len(files) != 2 {
		t.Fatalf("restart files=%+v %v", files, err)
	}
	if err := second.Delete(t.Context(), "tmp/file"); err != nil {
		t.Fatal(err)
	}
	files, _ = restarted.List(t.Context(), "tmp", true)
	if len(files) != 1 {
		t.Fatal("replica used stale catalog")
	}
}
