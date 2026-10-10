package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

type durableInput struct {
	Value string `json:"value"`
}
type durableOutput struct {
	Answer string `json:"answer"`
}

func fixtureModule(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/local-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	migrations := filepath.Join(dir, "db", "migrations")
	if err := os.MkdirAll(migrations, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migrations, "00001_fixture.sql"), []byte("-- +goose Up\nCREATE TABLE local_fixture (value text NOT NULL);\n-- +goose Down\nDROP TABLE local_fixture;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
}

func TestSharedLocalRuntimeNativeLifecycle(t *testing.T) {
	fixtureModule(t)
	var task *agentsdk.AgentHandle[durableInput, durableOutput]
	var job *agentsdk.JobHandle[durableInput, durableOutput]
	env := agenttest.NewWithOptions(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "Shared runtime fixture"})
		a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Fixture"})
		a.RegisterDirectory("internal", agentsdk.DirectoryOpts{Read: agentsdk.AccessInternal, Write: agentsdk.AccessInternal, List: agentsdk.AccessInternal, Description: "Internal fixture bytes"})
		task = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[durableInput, durableOutput]{Slug: "task", Description: "Fixture", Instructions: "Complete fixture", ModelSlot: "reasoning", MaxAttempts: 2, MaxConcurrency: 1})
		job = agentsdk.RegisterJob(a, &agentsdk.Job[durableInput, durableOutput]{Name: "save", Version: 1, Description: "Save fixture", Timeout: 5 * time.Second, MaxAttempts: 1, MaxConcurrency: 1, Handler: func(ctx context.Context, j agentsdk.JobContext, input durableInput) (durableOutput, error) {
			caller := agentsdk.CallerFromContext(ctx)
			if caller.Kind() != agentsdk.CallerApplication {
				return durableOutput{}, errors.New("wrong job caller")
			}
			if _, ok := caller.User(); ok {
				return durableOutput{}, errors.New("application job borrowed a human")
			}
			if _, err := a.DB().ExecContext(ctx, "INSERT INTO local_fixture (value) VALUES ($1)", input.Value); err != nil {
				return durableOutput{}, err
			}
			if _, err := a.WriteFile(ctx, "internal/value.txt", strings.NewReader(input.Value), "text/plain"); err != nil {
				return durableOutput{}, err
			}
			if err := j.ReportProgress(ctx, agentsdk.JobProgress{Phase: "saved", Completed: 1, Total: 1}); err != nil {
				return durableOutput{}, err
			}
			return durableOutput{Answer: input.Value}, nil
		}})
		return a
	}, agenttest.Options{Mocks: map[string]testutil.MockConfig{"reasoning": {Default: &testutil.MockResponse{ToolCalls: []stream.ToolCall{{ID: "complete", Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"answer":"fixture"}}`)}}, Usage: stream.UsageFrom(1, 1)}}}})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	id := uuid.NewString()
	started, err := task.Start(ctx, id, durableInput{Value: "first"})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := task.Wait(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != agentsdk.AgentRunCompleted || completed.Reply.Output.Answer != "fixture" {
		t.Fatalf("task=%+v", completed)
	}
	replay, err := task.Start(ctx, id, durableInput{Value: "first"})
	if err != nil || replay.Created || replay.ID != started.ID {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	next, err := task.Continue(ctx, completed.SessionID, uuid.NewString(), "continue fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.Wait(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	page, err := task.List(ctx, agentsdk.ListAgentRunsOptions{})
	if err != nil || len(page.Runs) != 2 {
		t.Fatalf("page=%+v %v", page, err)
	}
	queued, err := job.Enqueue(ctx, uuid.NewString(), durableInput{Value: "durable bytes"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		observed, err := job.Get(ctx, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Status == agentsdk.JobStatusSucceeded {
			if observed.Output.Answer != "durable bytes" || observed.Progress == nil || observed.Progress.Phase != "saved" {
				t.Fatalf("job=%+v", observed)
			}
			break
		}
		if observed.Status == agentsdk.JobStatusFailed {
			t.Fatalf("job failed: %s", observed.LastError)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	var count int
	if err := env.Agent.DB().QueryRowContext(ctx, "SELECT count(*) FROM local_fixture").Scan(&count); err != nil || count != 1 {
		t.Fatalf("database count=%d %v", count, err)
	}
	f, err := env.Agent.OpenFile(ctx, "internal/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil || string(raw) != "durable bytes" {
		t.Fatalf("storage=%q %v", raw, err)
	}
}

func TestSharedLocalRuntimeDenoPrivateTool(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 to run the packaged Deno fixture")
	}
	fixtureModule(t)
	image := "agentsdk-local-fixture:test"
	if err := jsexec.BuildImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	executor, err := agenttest.Executor(agenttest.ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	var handle *agentsdk.AgentHandle[durableInput, durableOutput]
	env := agenttest.NewWithOptions(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "Local Deno fixture"})
		a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Fixture"})
		a.RegisterDirectory("internal", agentsdk.DirectoryOpts{Read: agentsdk.AccessInternal, Write: agentsdk.AccessInternal, List: agentsdk.AccessInternal, Description: "Fixture bytes"})
		private := tool.New("save").Description("Persist fixture").SchemaFromStruct(durableInput{}).OutputSchemaFromStruct(durableOutput{}).Execute(func(ctx context.Context, raw json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
			caller := agentsdk.CallerFromContext(ctx)
			if caller.Kind() != agentsdk.CallerApplication {
				return tool.Result{}, errors.New("wrong private caller")
			}
			if _, ok := caller.User(); ok {
				return tool.Result{}, errors.New("private task borrowed a human")
			}
			var input durableInput
			if err := json.Unmarshal(raw, &input); err != nil {
				return tool.Result{}, err
			}
			if _, err := a.DB().ExecContext(ctx, "INSERT INTO local_fixture (value) VALUES ($1)", input.Value); err != nil {
				return tool.Result{}, err
			}
			if _, err := a.WriteFile(ctx, "internal/deno.txt", strings.NewReader(input.Value), "text/plain"); err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: `{"answer":"saved"}`}, nil
		}).Build()
		handle = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[durableInput, durableOutput]{Slug: "task", Description: "Fixture", Instructions: "Save then complete", ModelSlot: "reasoning", Tools: []tool.Tool{private}, MaxAttempts: 2, MaxConcurrency: 1})
		return a
	}, agenttest.Options{ExecutorFactory: executor, Mocks: map[string]testutil.MockConfig{"reasoning": {Responses: []testutil.MockResponse{
		{ToolCalls: []stream.ToolCall{{ID: "script", Name: "run_js", Input: json.RawMessage(`{"code":"return await tools.save({value:'actual Deno'});","description":"Persist fixture"}`)}}, Usage: stream.UsageFrom(1, 1)},
		{ToolCalls: []stream.ToolCall{{ID: "done", Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"answer":"saved"}}`)}}, Usage: stream.UsageFrom(1, 1)},
	}}}})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	started, err := handle.Start(ctx, uuid.NewString(), durableInput{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := handle.Wait(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != agentsdk.AgentRunCompleted {
		t.Fatalf("Deno task=%+v", out)
	}
	var value string
	if err := env.Agent.DB().QueryRowContext(ctx, "SELECT value FROM local_fixture").Scan(&value); err != nil || value != "actual Deno" {
		t.Fatalf("Deno DB effect=%q %v", value, err)
	}
	f, err := env.Agent.OpenFile(ctx, "internal/deno.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, _ := io.ReadAll(f)
	if string(raw) != "actual Deno" {
		t.Fatalf("Deno file=%q", raw)
	}
}
