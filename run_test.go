package agentsdk

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestConnectionHandleProxy(t *testing.T) {
	a, mock := testAgent(t)
	run := newRun(a, "run-1", "", "", context.Background())

	gmail := &ConnectionHandle{slug: "gmail", agent: a}
	_ = run // run not needed for handle methods
	result, err := gmail.Request(context.Background(), "GET", "/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Fatal("expected non-empty response")
	}

	reqs := mock.RequestsByPath("/api/agent/proxy/gmail")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 proxy request, got %d", len(reqs))
	}
}

func TestDirectoryWriteAndRead(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("uploads", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser})

	if _, err := a.WriteFile(context.Background(), "uploads/test.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}

	rc, err := a.OpenFile(context.Background(), "uploads/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "mock-file-content" {
		t.Fatalf("expected mock content, got %q", string(data))
	}
}

// TestBackgroundRun exercises the rolling ambient run that backs
// `agent.LLM` calls made with a ctx that has no dispatcher-bound run.
func TestBackgroundRun(t *testing.T) {
	a, mock := testAgent(t)

	// agent.LLM with plain ctx triggers background run creation.
	m := a.LLM(context.Background(), "summarize", ModelOpts{})
	events, err := m.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	createReqs := mock.RequestsByPath("/api/agent/run/create")
	if len(createReqs) != 1 {
		t.Fatalf("expected 1 run/create request, got %d", len(createReqs))
	}
	var createBody CreateRunRequest
	if err := json.Unmarshal(createReqs[0].Body, &createBody); err != nil {
		t.Fatal(err)
	}
	if createBody.TriggerType != "background" {
		t.Errorf("trigger_type = %q, want background", createBody.TriggerType)
	}

	// A second call within the inactivity window reuses the same run —
	// no additional run/create request.
	m2 := a.LLM(context.Background(), "analyze", ModelOpts{})
	events2, _ := m2.Stream(context.Background(), nil)
	for range events2 {
	}
	createReqs = mock.RequestsByPath("/api/agent/run/create")
	if len(createReqs) != 1 {
		t.Fatalf("expected background run reuse; got %d create requests", len(createReqs))
	}

	// Force-flush for test cleanup.
	a.stopBackgroundFlusher()
	completeReqs := mock.RequestsByPath("/api/agent/run/complete")
	if len(completeReqs) == 0 {
		t.Fatal("expected background run to flush on stop")
	}
}
