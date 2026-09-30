package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

func TestRegisterAsyncToolEnqueuesBeforeReturningHandle(t *testing.T) {
	a, mock := testAgent(t)
	definition := testJobDefinition(1)
	executed := false
	definition.Handler = func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
		executed = true
		return testJobOutput{}, nil
	}
	RegisterAsyncTool(a, &AsyncTool[testJobInput, testJobOutput]{Job: definition, Access: AccessUser})
	manifest := a.Manifest()
	if len(manifest.Tools) != 1 || len(manifest.JobHandlers) != 1 || manifest.JobHandlers[0].Name != definition.Name {
		t.Fatalf("async declaration: %+v", manifest)
	}
	description := manifest.Tools[0].Description
	if !strings.Contains(description, "exact fields {id, status}") || strings.Contains(description, "tasks.wait") {
		t.Fatalf("async tool description = %q", description)
	}
	result, err := a.tools[definition.Name].Execute(t.Context(), json.RawMessage(`{"source":"uploads/movie.mp4"}`), tool.CallOptions{ToolCallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	var task AsyncTask
	if err := json.Unmarshal([]byte(result.Output), &task); err != nil {
		t.Fatal(err)
	}
	requests := mock.RequestsByPath("/api/agent/jobs")
	if len(requests) != 1 || executed {
		t.Fatalf("job admission requests=%d handler executed=%t", len(requests), executed)
	}
	var enqueued wire.EnqueueJobRequest
	if err := json.Unmarshal(requests[0].Body, &enqueued); err != nil {
		t.Fatal(err)
	}
	if task.ID != "job:"+enqueued.ID || task.Status != JobStatusQueued || enqueued.Name != definition.Name || enqueued.Version != int32(definition.Version) || !strings.Contains(string(enqueued.Input), "uploads/movie.mp4") {
		t.Fatalf("task=%+v request=%+v", task, enqueued)
	}
}

func TestRegisterAsyncToolRequiresPolicy(t *testing.T) {
	for _, access := range []Access{"", "invalid"} {
		t.Run(string(access), func(t *testing.T) {
			a, _ := testAgent(t)
			defer func() {
				if recover() == nil {
					t.Fatal("missing access policy was accepted")
				}
			}()
			RegisterAsyncTool(a, &AsyncTool[testJobInput, testJobOutput]{Job: testJobDefinition(1), Access: access})
		})
	}
}
