package agenttest_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func TestMockAgentResponses(t *testing.T) {
	m, base := agenttest.NewMockAirlock()
	defer m.Close()
	path := "/api/agent/agents/worker/runs?limit=2"
	if err := m.SetAgentResponse("GET", path, 200, wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{}, NextCursor: "next"}); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), "GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test-token")
	resp, err := m.Server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page wire.ListAgentRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || page.NextCursor != "next" {
		t.Fatalf("status=%d page=%+v", resp.StatusCode, page)
	}
	requests := m.Requests()
	if len(requests) != 1 || requests[0].Header.Get("Authorization") != "Bearer test-token" {
		t.Fatalf("requests=%+v", requests)
	}
	r.Header.Set("Authorization", "mutated")
	requests[0].Header.Set("Authorization", "mutated")
	if m.Requests()[0].Header.Get("Authorization") != "Bearer test-token" {
		t.Fatal("request headers alias mock state")
	}
	r.URL.RawQuery = ""
	missing, err := m.Server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != 500 {
		t.Fatalf("unconfigured task response status=%d", missing.StatusCode)
	}
}

func TestAgentLifecycleWithStartedSDK(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/task-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "db", "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)
	type input struct {
		Task string `json:"task"`
	}
	type output struct {
		Answer string `json:"answer"`
	}
	var h *agentsdk.AgentHandle[input, output]
	env := agenttest.New(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "Task test"})
		a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Reason"})
		h = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[input, output]{Slug: "worker", Description: "Work", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
		return a
	})
	run := wire.AgentRunInfo{ID: uuid.NewString(), SessionID: uuid.NewString(), Definition: h.Slug(), ContractHash: h.ContractHash(), Status: "queued"}
	if err := env.Airlock.SetAgentResponse("POST", "/api/agent/agents/worker/runs", 200, wire.AgentRunResponse{Run: run, Created: true}); err != nil {
		t.Fatal(err)
	}
	env.Airlock.Reset()
	ctx := agenttest.WithUser(t.Context(), agentsdk.User{ID: uuid.NewString()})
	result, err := h.Start(ctx, uuid.NewString(), input{Task: "Work"})
	if err != nil || result.ID != run.ID || !result.Created {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	requests := env.Airlock.Requests()
	if len(requests) != 1 || requests[0].Header.Get("X-Airlock-Run-ID") != "" {
		t.Fatalf("requests=%+v", requests)
	}
}
