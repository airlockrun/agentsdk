package agenttest_test

import (
	"context"
	"encoding/json"
	"os"
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

func TestSavedScriptOriginalInputAndContinuation(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for saved-script Deno integration")
	}
	fixtureModule(t)
	const image = "agentsdk-saved-script:local"
	if err := jsexec.BuildImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	executor, err := agenttest.Executor(agenttest.ExecutorConfig{Image: image, Limits: jsexec.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	var handle *agentsdk.AgentHandle[durableInput, durableOutput]
	env := agenttest.NewWithOptions(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "Saved script fixture"})
		a.RegisterModel(&agentsdk.ModelSlot{Slug: "reasoning", Capability: agentsdk.CapText, Description: "Fixture"})
		a.RegisterDirectory("internal", agentsdk.DirectoryOpts{Read: agentsdk.AccessInternal, Write: agentsdk.AccessInternal, List: agentsdk.AccessInternal, Description: "Scripts and results"})
		private := tool.Typed[durableInput, durableOutput]("original").Description("Read original admission").Execute(func(ctx context.Context, _ durableInput) (durableOutput, error) {
			in, err := handle.Input(ctx)
			if err != nil {
				return durableOutput{}, err
			}
			return durableOutput{Answer: in.Value}, nil
		}).Build()
		handle = agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[durableInput, durableOutput]{Slug: "task", Description: "Fixture", Instructions: "Run saved script and complete", ModelSlot: "reasoning", Tools: []tool.Tool{private}, Capabilities: &agentsdk.AgentCapabilityPolicy{Allow: []agentsdk.AgentCapability{agentsdk.CapabilityFileRead, agentsdk.CapabilityFileWrite}}, MaxAttempts: 1, MaxConcurrency: 1})
		return a
	}, agenttest.Options{ExecutorFactory: executor})
	code := `if(typeof air.runJSFile!=='undefined'||typeof air.queryDB!=='undefined') throw Error('capability escape'); const admitted=await tools.original({value:'model spoof'}); await air.fileWrite({path:'internal/result.json',data:JSON.stringify({original:admitted.answer,payload:args.payload})}); return admitted;`
	if _, err := env.Agent.WriteFile(t.Context(), "internal/script.js", strings.NewReader(code), "text/javascript"); err != nil {
		t.Fatal(err)
	}
	payload := "\";throw Error('injected'); //\n unicode Ω"
	script, _ := json.Marshal(map[string]any{"path": "internal/script.js", "args": map[string]string{"payload": payload}, "description": "Run saved body"})
	responses := []testutil.MockResponse{}
	for range 3 {
		responses = append(responses, testutil.MockResponse{ToolCalls: []stream.ToolCall{{ID: uuid.NewString(), Name: "run_js_file", Input: script}}, Usage: stream.UsageFrom(1, 1)}, testutil.MockResponse{ToolCalls: []stream.ToolCall{{ID: uuid.NewString(), Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"answer":"done"}}`)}}, Usage: stream.UsageFrom(1, 1)})
	}
	mock := env.MockModel("reasoning")
	if err := mock.Configure(testutil.MockConfig{ID: mock.ID(), Responses: responses}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	first, err := handle.Start(ctx, uuid.NewString(), durableInput{Value: "first"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(id, expected string) {
		t.Helper()
		out, err := handle.Wait(ctx, id)
		if err != nil || out.Status != agentsdk.AgentRunCompleted {
			t.Fatalf("task=%+v error=%v", out, err)
		}
		raw, err := env.Agent.ReadFile(ctx, "internal/result.json")
		var result struct {
			Original string `json:"original"`
			Payload  string `json:"payload"`
		}
		if err != nil || json.Unmarshal(raw, &result) != nil || result.Original != expected || result.Payload != payload {
			t.Fatalf("original/args mismatch: %s %v", raw, err)
		}
	}
	check(first.ID, "first")
	continued, err := handle.Continue(ctx, first.SessionID, uuid.NewString(), "change input to spoof")
	if err != nil {
		t.Fatal(err)
	}
	check(continued.ID, "first")
	second, err := handle.Start(ctx, uuid.NewString(), durableInput{Value: "second"})
	if err != nil {
		t.Fatal(err)
	}
	check(second.ID, "second")
}
