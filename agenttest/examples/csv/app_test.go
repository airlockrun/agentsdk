package csvexample_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
	csvexample "github.com/airlockrun/agentsdk/agenttest/examples/csv"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
)

// Only this test package opts into flags. Importing the app does not register any.
var modelOptions = agenttest.RegisterFlags(flag.CommandLine)

func TestConversion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	// The SDK finds the enclosing source module's db/migrations during startup.
	// This checked-in fixture has no app database tables to migrate.
	t.Chdir(filepath.Join("testdata", "app"))
	outputDir := t.TempDir()
	var handle *agentsdk.AgentHandle[csvexample.Input, csvexample.Output]
	env := agenttest.NewWithOptions(t, func() *agentsdk.Agent {
		app, task := csvexample.NewApp(outputDir)
		handle = task
		return app
	}, *modelOptions)
	input := csvexample.Input{
		Header: []string{"name", "note", "quantity"},
		Rows: [][]string{
			{"Alice", `comma, quote "ok"`, "2"},
			{"Bob", "two\nlines", "3"},
			{"Саша", "Unicode", "4"},
		},
	}
	configureMock(t, env, input)
	factory := executor(t, ctx)
	result, err := agenttest.RunAgent(t, ctx, env, handle, input, agenttest.RunOptions{
		ExecutorFactory: factory, MaxSteps: 8, Timeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("run conversion: %v", err)
	}
	if result.Reply == nil || result.Reply.Kind != agentsdk.AgentReplyOutput || result.Reply.Output == nil {
		t.Fatalf("expected typed output completion, got %+v", result.Reply)
	}
	wantOutput := csvexample.Output{Filename: csvexample.Filename, Rows: len(input.Rows)}
	if *result.Reply.Output != wantOutput {
		t.Fatalf("typed output = %+v, want %+v", *result.Reply.Output, wantOutput)
	}
	if result.Runtime.CleanupError != nil {
		t.Fatalf("executor cleanup: %v", result.Runtime.CleanupError)
	}
	if len(result.AppCalls) == 0 {
		t.Fatal("task did not invoke its private app tool")
	}
	artifact := filepath.Join(outputDir, csvexample.Filename)
	file, err := os.Open(artifact)
	if err != nil {
		t.Fatalf("open CSV artifact: %v", err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV artifact: %v", err)
	}
	wantRows := append([][]string{input.Header}, input.Rows...)
	if !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("CSV records = %#v, want %#v", rows, wantRows)
	}
	t.Logf("CSV artifact verified: %s (%d data rows); typed completion received", artifact, len(input.Rows))
}

func configureMock(t *testing.T, env *agenttest.Env, input csvexample.Input) {
	t.Helper()
	rawInput, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	script, err := json.Marshal(map[string]string{
		"code":        "return await tools.write_csv(" + string(rawInput) + ");",
		"description": "Write structured rows to CSV",
	})
	if err != nil {
		t.Fatal(err)
	}
	completion, err := json.Marshal(map[string]any{
		"kind": "output", "output": csvexample.Output{Filename: csvexample.Filename, Rows: len(input.Rows)},
	})
	if err != nil {
		t.Fatal(err)
	}
	mock := env.MockModel("conversion")
	// Configure is valid even when the slot is live: that mock remains dormant.
	if err := mock.Configure(testutil.MockConfig{ID: mock.ID(), Responses: []testutil.MockResponse{
		{ToolCalls: []stream.ToolCall{{ID: "mock-script", Name: "run_js", Input: script}}, Usage: stream.UsageFrom(10, 5)},
		{ToolCalls: []stream.ToolCall{{ID: "mock-complete", Name: "complete", Input: completion}}, Usage: stream.UsageFrom(10, 5)},
	}}); err != nil {
		t.Fatal(err)
	}
}

func executor(t *testing.T, ctx context.Context) chatruntime.ExecutorFactory {
	t.Helper()
	var config agenttest.ExecutorConfig
	if os.Getenv("AIRLOCK_TEST_EXECUTOR_URL") != "" || os.Getenv("AIRLOCK_TEST_EXECUTOR_TOKEN") != "" {
		var err error
		config, err = agenttest.ExecutorConfigFromEnv(jsexec.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
	} else {
		image := os.Getenv("TEST_JSEXEC_IMAGE")
		if image == "" {
			image = "agentsdk-csv-example:local"
			if err := jsexec.BuildImage(ctx, image); err != nil {
				t.Fatal(err)
			}
		}
		config = agenttest.ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits()}
	}
	factory, err := agenttest.Executor(config)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}
