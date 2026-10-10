package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func taskRun(h *AgentHandle[taskInput, taskOutput]) wire.AgentRunInfo {
	return wire.AgentRunInfo{ID: uuid.NewString(), SessionID: uuid.NewString(), Definition: h.Slug(), ContractHash: h.ContractHash(), Status: "completed",
		Reply: &wire.AgentReply{Kind: "output", Output: json.RawMessage(`{"answer":"done"}`)}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Steps: 2, Tokens: 30}
}

// protocolHost asserts exact HTTP exchanges for production AgentHandle client
// tests. It is test-owned transport plumbing, not a task execution service.
type protocolHost struct {
	server      *httptest.Server
	mu          sync.Mutex
	method, uri string
	status      int
	body        []byte
	requests    []protocolRequest
}

type protocolRequest struct {
	Method, Path string
	Body         []byte
	Header       http.Header
}

func protocolAgent(t *testing.T) (*Agent, *protocolHost) {
	t.Helper()
	host := &protocolHost{}
	host.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read request", 400)
			return
		}
		host.mu.Lock()
		host.requests = append(host.requests, protocolRequest{Method: r.Method, Path: r.URL.RequestURI(), Body: body, Header: r.Header.Clone()})
		method, uri, status, reply := host.method, host.uri, host.status, append([]byte(nil), host.body...)
		host.mu.Unlock()
		if r.Method != method || r.URL.RequestURI() != uri {
			t.Errorf("protocol request = %s %s, want %s %s", r.Method, r.URL.RequestURI(), method, uri)
			http.Error(w, "unexpected protocol request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}))
	t.Cleanup(host.server.Close)
	return newTestAgent(t, host.server.URL), host
}

func (h *protocolHost) respond(method, uri string, status int, reply any) error {
	body, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.method, h.uri, h.status, h.body = method, uri, status, body
	return nil
}
func (h *protocolHost) Requests() []protocolRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocolRequest(nil), h.requests...)
}
func (h *protocolHost) RequestsByPath(prefix string) []protocolRequest {
	var selected []protocolRequest
	for _, req := range h.Requests() {
		if strings.HasPrefix(req.Path, prefix) {
			selected = append(selected, req)
		}
	}
	return selected
}
func (h *protocolHost) Reset() { h.mu.Lock(); defer h.mu.Unlock(); h.requests = nil }

func TestAgentHandleIDLifecycle(t *testing.T) {
	a, mock := protocolAgent(t)
	taskModel(a)
	h := RegisterAgent(a, taskDefinition())
	run := taskRun(h)
	base := "/api/agent/agents/worker"
	requestID := uuid.NewString()
	set := func(method, path string, body any) {
		t.Helper()
		if err := mock.respond(method, base+path, 200, body); err != nil {
			t.Fatal(err)
		}
	}
	set("POST", "/runs", wire.AgentRunResponse{Run: run, Created: true})
	ctx := testcaller.With(t.Context(), testWireCaller("user", wire.AccessUser))
	info, err := h.Start(ctx, requestID, taskInput{Task: "work"})
	if err != nil || !info.Created || info.Reply == nil || info.Reply.Output.Answer != "done" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	set("POST", "/runs", wire.AgentRunResponse{Run: run})
	retry, err := h.Start(ctx, requestID, taskInput{Task: "work"})
	if err != nil || retry.Created || retry.ID != run.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	requests := mock.Requests()
	if len(requests) != 2 || string(requests[0].Body) != string(requests[1].Body) {
		t.Fatal("retry request changed or a source run was minted")
	}
	var start wire.StartAgentRequest
	if err := json.Unmarshal(requests[0].Body, &start); err != nil {
		t.Fatal(err)
	}
	if start.RequestID != requestID || start.ContractHash != h.ContractHash() || string(start.Input) != `{"task":"work"}` {
		t.Fatalf("request=%+v", start)
	}
	for _, req := range requests {
		if req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("X-Airlock-Run-ID") != "" || req.Header.Get(wire.CallerHeader) != "" {
			t.Fatalf("unexpected attribution: %v", req.Header)
		}
	}
	// Reconstruct a handle from the same declarations, not a process-local call object.
	recovered := &AgentHandle[taskInput, taskOutput]{agent: a, slug: h.Slug()}
	set("GET", "/runs/"+run.ID, wire.AgentRunResponse{Run: run})
	if got, err := recovered.Get(t.Context(), run.ID); err != nil || got.ID != run.ID {
		t.Fatalf("Get=%+v %v", got, err)
	}
	if got, err := recovered.Wait(t.Context(), run.ID); err != nil || got.Status != AgentRunCompleted {
		t.Fatalf("Wait=%+v %v", got, err)
	}
	set("GET", "/runs?contractHash="+h.ContractHash()+"&cursor=a%2Bb&limit=2&sessionId="+run.SessionID+"&status=completed", wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{run}, NextCursor: "next"})
	page, err := recovered.List(t.Context(), ListAgentRunsOptions{Limit: 2, Cursor: "a+b", SessionID: run.SessionID, Status: AgentRunCompleted})
	if err != nil || len(page.Runs) != 1 || page.NextCursor != "next" {
		t.Fatalf("List=%+v %v", page, err)
	}
	for _, kind := range []string{"output", "needs_input"} {
		run.Reply = &wire.AgentReply{Kind: kind, Output: json.RawMessage(`{"answer":"done"}`)}
		if kind == "needs_input" {
			run.Reply.Output = nil
			run.Reply.Question = "Which task?"
		}
		run.ID = uuid.NewString()
		set("POST", "/sessions/"+run.SessionID+"/continue", wire.AgentRunResponse{Run: run, Created: true})
		info, err := recovered.Continue(t.Context(), run.SessionID, uuid.NewString(), "Keep working")
		if err != nil || string(info.Reply.Kind) != kind || info.SessionID != run.SessionID {
			t.Fatalf("Continue=%+v %v", info, err)
		}
	}
	run.Status = "cancelled"
	run.Reply = nil
	set("DELETE", "/runs/"+run.ID, wire.AgentRunResponse{Run: run})
	if got, err := recovered.Cancel(t.Context(), run.ID); err != nil || got.Status != AgentRunCancelled {
		t.Fatalf("Cancel=%+v %v", got, err)
	}
	if len(mock.RequestsByPath("/api/agent/run/")) != 0 {
		t.Fatal("lifecycle materialized SDK rolling run")
	}
}

func TestAgentHandleListCurrentContract(t *testing.T) {
	oldApp, mock := protocolAgent(t)
	taskModel(oldApp)
	old := RegisterAgent(oldApp, taskDefinition())
	currentApp, _ := testAgent(t)
	taskModel(currentApp)
	definition := taskDefinition()
	definition.Instructions += " Include sources."
	current := RegisterAgent(currentApp, definition)
	currentApp.client = oldApp.client
	if old.ContractHash() == current.ContractHash() {
		t.Fatal("definition change retained contract hash")
	}
	oldRun, currentRun := taskRun(old), taskRun(current)
	for _, h := range []*AgentHandle[taskInput, taskOutput]{old, current} {
		run := oldRun
		if h == current {
			run = currentRun
		}
		path := "/api/agent/agents/worker/runs?contractHash=" + h.ContractHash()
		if err := mock.respond("GET", path, 200, wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{run}, NextCursor: "next"}); err != nil {
			t.Fatal(err)
		}
		page, err := h.List(t.Context(), ListAgentRunsOptions{})
		if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != run.ID {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		if err := mock.respond("GET", path+"&cursor=next&limit=1", 200, wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{}}); err != nil {
			t.Fatal(err)
		}
		page, err = h.List(t.Context(), ListAgentRunsOptions{Cursor: page.NextCursor, Limit: 1})
		if err != nil || len(page.Runs) != 0 {
			t.Fatalf("next page=%+v err=%v", page, err)
		}
	}
	// An incorrectly filtered host page still fails strict result validation.
	path := "/api/agent/agents/worker/runs?contractHash=" + current.ContractHash()
	if err := mock.respond("GET", path, 200, wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{oldRun}}); err != nil {
		t.Fatal(err)
	}
	if _, err := current.List(t.Context(), ListAgentRunsOptions{}); err == nil || !strings.Contains(err.Error(), "contract mismatch") {
		t.Fatalf("historical result error=%v", err)
	}
}

func TestAgentHandleOutputConstraints(t *testing.T) {
	t.Run("fixed array", testAgentOutputConstraints[[1]string])
	t.Run("int8", testAgentOutputConstraints[int8])
	t.Run("nested", testAgentOutputConstraints[struct {
		Values *[1][2]int8 `json:"values"`
	}])
}

func testAgentOutputConstraints[Out any](t *testing.T) {
	a, mock := protocolAgent(t)
	taskModel(a)
	h := RegisterAgent(a, &AgentDefinition[taskInput, Out]{Slug: "worker", Description: "Work", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
	var cases []struct {
		raw   string
		valid bool
	}
	switch any(*new(Out)).(type) {
	case [1]string:
		cases = []struct {
			raw   string
			valid bool
		}{{`["one"]`, true}, {`["one","two"]`, false}, {`[]`, false}}
	case int8:
		cases = []struct {
			raw   string
			valid bool
		}{{`127`, true}, {`-128`, true}, {`128`, false}, {`-129`, false}}
	default:
		cases = []struct {
			raw   string
			valid bool
		}{{`{"values":[[127,-128]]}`, true}, {`{"values":null}`, true}, {`{"values":[[1,2],[3,4]]}`, false}, {`{"values":[[1,2,3]]}`, false}, {`{"values":[[128,0]]}`, false}, {`{"VALUES":[[1,2,3]]}`, false}}
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			run := wire.AgentRunInfo{ID: uuid.NewString(), SessionID: uuid.NewString(), Definition: h.Slug(), ContractHash: h.ContractHash(), Status: "completed", Reply: &wire.AgentReply{Kind: "output", Output: json.RawMessage(tc.raw)}}
			if err := mock.respond("GET", "/api/agent/agents/worker/runs/"+run.ID, 200, wire.AgentRunResponse{Run: run}); err != nil {
				t.Fatal(err)
			}
			info, err := h.Get(t.Context(), run.ID)
			if tc.valid {
				if err != nil || info.Reply == nil || info.Reply.Output == nil {
					t.Fatalf("info=%+v err=%v", info, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "validate agent output") {
				t.Fatalf("must fail before typed decode: info=%+v err=%v", info, err)
			}
		})
	}
}

func TestAgentHandleStrictClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*wire.AgentRunInfo)
	}{
		{"definition", func(r *wire.AgentRunInfo) { r.Definition = "other" }},
		{"hash", func(r *wire.AgentRunInfo) { r.ContractHash = "other" }},
		{"wrong ID", func(r *wire.AgentRunInfo) { r.ID = uuid.NewString() }},
		{"invalid session ID", func(r *wire.AgentRunInfo) { r.SessionID = "bad" }},
		{"status", func(r *wire.AgentRunInfo) { r.Status = "yielded" }},
		{"missing reply", func(r *wire.AgentRunInfo) { r.Reply = nil }},
		{"unknown kind", func(r *wire.AgentRunInfo) { r.Reply.Kind = "unknown" }},
		{"output absent", func(r *wire.AgentRunInfo) { r.Reply.Output = nil }},
		{"output type", func(r *wire.AgentRunInfo) { r.Reply.Output = json.RawMessage(`42`) }},
		{"null output", func(r *wire.AgentRunInfo) { r.Reply.Output = json.RawMessage(`null`) }},
		{"unknown output field", func(r *wire.AgentRunInfo) { r.Reply.Output = json.RawMessage(`{"answer":"done","extra":true}`) }},
		{"mixed reply", func(r *wire.AgentRunInfo) { r.Reply.Question = "question" }},
		{"empty question", func(r *wire.AgentRunInfo) { r.Reply = &wire.AgentReply{Kind: "needs_input", Question: " "} }},
		{"reply before completion", func(r *wire.AgentRunInfo) { r.Status = "running" }},
		{"negative usage", func(r *wire.AgentRunInfo) { r.Tokens = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, mock := protocolAgent(t)
			taskModel(a)
			h := RegisterAgent(a, taskDefinition())
			r := taskRun(h)
			id := r.ID
			tc.edit(&r)
			if err := mock.respond("GET", "/api/agent/agents/worker/runs/"+id, 200, wire.AgentRunResponse{Run: r}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Get(t.Context(), id); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
	for _, body := range []string{"", `{}`, `{"run":`, `null`} {
		t.Run("body="+body, func(t *testing.T) {
			a, _ := testAgent(t)
			taskModel(a)
			h := RegisterAgent(a, taskDefinition())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			a.client = newAirlockClient(server.URL, a.token, server.Client())
			if _, err := h.Get(t.Context(), uuid.NewString()); err == nil {
				t.Fatal("empty or invalid response accepted")
			}
			if _, err := h.List(t.Context(), ListAgentRunsOptions{}); err == nil {
				t.Fatal("empty or invalid list response accepted")
			}
		})
	}
}

func TestAgentHandleWaitCancellationAndFailures(t *testing.T) {
	a, mock := protocolAgent(t)
	taskModel(a)
	h := RegisterAgent(a, taskDefinition())
	r := taskRun(h)
	r.Status = "waiting"
	r.Reply = nil
	path := "/api/agent/agents/worker/runs/" + r.ID
	if err := mock.respond("GET", path, 200, wire.AgentRunResponse{Run: r}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.Wait(ctx, r.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait error=%v", err)
	}
	for _, req := range mock.Requests() {
		if req.Method != "GET" {
			t.Fatalf("Wait performed %s", req.Method)
		}
	}
	for _, status := range []string{"failed", "cancelled", "budget_exceeded"} {
		r.Status = status
		r.Error = "stopped"
		if err := mock.respond("GET", path, 200, wire.AgentRunResponse{Run: r}); err != nil {
			t.Fatal(err)
		}
		got, err := h.Wait(t.Context(), r.ID)
		if err != nil || string(got.Status) != status || got.Error != "stopped" {
			t.Fatalf("state=%+v err=%v", got, err)
		}
	}
	if err := mock.respond("GET", path, 409, map[string]string{"error": "incompatible deployment"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Get(t.Context(), r.ID); err == nil || !strings.Contains(err.Error(), "incompatible deployment") {
		t.Fatalf("host error=%v", err)
	}
	mock.Reset()
	if _, err := h.Start(t.Context(), "bad", taskInput{}); err == nil {
		t.Fatal("invalid start UUID accepted")
	}
	if _, err := h.Get(t.Context(), "../x"); err == nil {
		t.Fatal("invalid get UUID accepted")
	}
	if _, err := h.Cancel(t.Context(), "bad"); err == nil {
		t.Fatal("invalid cancel UUID accepted")
	}
	if _, err := h.Continue(t.Context(), r.SessionID, uuid.NewString(), " "); err == nil {
		t.Fatal("blank continuation accepted")
	}
	if _, err := h.List(t.Context(), ListAgentRunsOptions{Status: "yielded"}); err == nil {
		t.Fatal("invalid filter accepted")
	}
	if len(mock.Requests()) != 0 {
		t.Fatal("invalid inputs made requests")
	}
}
