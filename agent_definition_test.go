package agentsdk

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

type taskInput struct {
	Task string `json:"task"`
}
type taskOutput struct {
	Answer string `json:"answer"`
}

func taskDefinition() *AgentDefinition[taskInput, taskOutput] {
	return &AgentDefinition[taskInput, taskOutput]{Slug: "worker", Description: "Work", Instructions: "Complete the task", ModelSlot: "reasoning", MaxAttempts: 2, MaxConcurrency: 3}
}

func taskModel(a *Agent) {
	a.RegisterModel(&ModelSlot{Slug: "reasoning", Capability: CapText, Description: "Task reasoning"})
}

func TestRegisterAgentManifest(t *testing.T) {
	a, mock := testAgent(t)
	taskModel(a)
	mcp := a.RegisterMCP(&MCP{Slug: "private", Name: "Private", URL: "https://example.com/mcp", AuthMode: MCPAuthNone})
	d := taskDefinition()
	d.Budget = &AgentBudget{Timeout: 30 * time.Minute, Tokens: 1000}
	d.MCPs = []*MCPHandle{mcp}
	d.Tools = []tool.Tool{tool.Typed[taskInput, taskOutput]("lookup").Description("Look up a task").Execute(func(context.Context, taskInput) (taskOutput, error) { return taskOutput{}, nil }).Build()}
	d.Tools[0].InputExamples = []tool.ToolInputExample{{Input: json.RawMessage(`{"task":"example"}`)}}
	h := RegisterAgent(a, d)
	before := a.buildManifest()
	if h.Slug() != "worker" || h.ContractHash() != before.AgentDefinitions[0].ContractHash {
		t.Fatal("handle contract mismatch")
	}
	d.Instructions = "mutated"
	d.Budget.Tokens = 1
	d.Budget.Timeout = time.Millisecond
	d.MCPs[0] = nil
	d.Tools[0].InputSchema[0] = '!'
	d.Tools[0].InputExamples[0].Input[0] = '!'
	manifest := a.Manifest()
	if !reflect.DeepEqual(before, manifest) {
		t.Fatal("registration retained mutable author state")
	}
	if len(manifest.Tools) != 0 || len(manifest.AgentDefinitions[0].Tools) != 1 || manifest.AgentDefinitions[0].Budget.Steps != 0 {
		t.Fatal("private tools leaked or omitted budget limits defaulted")
	}
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.AgentDefinitions[0].Tools[0].InputSchema[0] = '!'
	manifest.AgentDefinitions[0].MCPs[0] = "mutated"
	if !reflect.DeepEqual(before, a.Manifest()) {
		t.Fatal("manifest aliases registration state")
	}
	if err := a.syncWithAirlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	requests := mock.RequestsByPath("/api/agent/sync")
	var synced wire.AgentManifest
	if len(requests) != 1 {
		t.Fatalf("sync requests=%d", len(requests))
	}
	if err := json.Unmarshal(requests[0].Body, &synced); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.AgentDefinitions, synced.AgentDefinitions) {
		t.Fatal("sync declaration differs")
	}
}

func TestRegisterAgentValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Agent, *AgentDefinition[taskInput, taskOutput])
	}{
		{"nil definition", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) {
			RegisterAgent[taskInput, taskOutput](a, nil)
		}},
		{"missing model", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.ModelSlot = "missing" }},
		{"non-text model", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { a.modelSlots[0].Capability = CapVision }},
		{"missing instructions", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.Instructions = "" }},
		{"zero budget", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.Budget = &AgentBudget{} }},
		{"negative budget", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.Budget = &AgentBudget{Steps: -1} }},
		{"attempts", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.MaxAttempts = 0 }},
		{"concurrency", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.MaxConcurrency = 0 }},
		{"overflow", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.MaxAttempts = 1 << 32 }},
		{"duplicate", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { RegisterAgent(a, d) }},
		{"nil MCP", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { d.MCPs = []*MCPHandle{nil} }},
		{"foreign child", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) {
			other := New(Config{Description: "Other"})
			taskModel(other)
			d.Subagents = []Subagent{RegisterAgent(other, taskDefinition())}
		}},
		{"nil child", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) {
			var h *AgentHandle[taskInput, taskOutput]
			d.Subagents = []Subagent{h}
		}},
		{"private tool executor", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) {
			d.Tools = []tool.Tool{{Name: "bad", Description: "Bad"}}
		}},
		{"frozen", func(a *Agent, d *AgentDefinition[taskInput, taskOutput]) { a.Manifest() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{Description: "Test"})
			taskModel(a)
			d := taskDefinition()
			defer func() {
				if recover() == nil {
					t.Fatal("invalid registration accepted")
				}
			}()
			tc.edit(a, d)
			RegisterAgent(a, d)
		})
	}
}

func TestRegisterAgentBudgetConversion(t *testing.T) {
	maxExact := time.Duration(math.MaxInt64).Truncate(time.Millisecond)
	overflow := maxExact.Milliseconds() + 1
	for _, tc := range []struct {
		name          string
		budget        *AgentBudget
		want          wire.AgentBudget
		panicContains string
	}{
		{name: "omitted", want: wire.AgentBudget{Steps: 150}},
		{name: "explicit steps", budget: &AgentBudget{Steps: 20}, want: wire.AgentBudget{Steps: 20}},
		{name: "tokens only", budget: &AgentBudget{Tokens: 1000}, want: wire.AgentBudget{Tokens: 1000}},
		{name: "minimum timeout", budget: &AgentBudget{Timeout: time.Millisecond}, want: wire.AgentBudget{TimeoutMS: 1}},
		{name: "all limits", budget: &AgentBudget{Steps: 100, Timeout: 30 * time.Minute, Tokens: 50000}, want: wire.AgentBudget{Steps: 100, TimeoutMS: 1800000, Tokens: 50000}},
		{name: "maximum exact timeout", budget: &AgentBudget{Timeout: maxExact}, want: wire.AgentBudget{TimeoutMS: math.MaxInt64 / int64(time.Millisecond)}},
		{name: "all zero", budget: &AgentBudget{}, panicContains: "at least one positive limit"},
		{name: "negative steps", budget: &AgentBudget{Steps: -1}, panicContains: "nonnegative limits"},
		{name: "negative tokens", budget: &AgentBudget{Tokens: -1}, panicContains: "nonnegative limits"},
		{name: "negative timeout", budget: &AgentBudget{Steps: 1, Timeout: -time.Millisecond}, panicContains: "Budget.Timeout must not be negative"},
		{name: "negative submillisecond", budget: &AgentBudget{Steps: 1, Timeout: -time.Nanosecond}, panicContains: "Budget.Timeout must not be negative"},
		{name: "positive submillisecond", budget: &AgentBudget{Steps: 1, Timeout: time.Nanosecond}, panicContains: "exact whole number of milliseconds"},
		{name: "fractional milliseconds", budget: &AgentBudget{Timeout: time.Millisecond + time.Nanosecond}, panicContains: "exact whole number of milliseconds"},
		{name: "maximum duration fractional", budget: &AgentBudget{Timeout: time.Duration(math.MaxInt64)}, panicContains: "exact whole number of milliseconds"},
		{name: "minimum duration", budget: &AgentBudget{Timeout: time.Duration(math.MinInt64)}, panicContains: "Budget.Timeout must not be negative"},
		{name: "overflowed millisecond construction", budget: &AgentBudget{Timeout: time.Duration(overflow) * time.Millisecond}, panicContains: "Budget.Timeout must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{Description: "Test budget"})
			taskModel(a)
			d := taskDefinition()
			d.Budget = tc.budget
			if tc.panicContains != "" {
				defer func() {
					value := recover()
					message, ok := value.(string)
					if !ok || !strings.Contains(message, tc.panicContains) {
						t.Fatalf("panic=%v, want %q", value, tc.panicContains)
					}
				}()
			}
			h := RegisterAgent(a, d)
			if tc.panicContains != "" {
				t.Fatal("invalid budget accepted")
			}
			def := a.Manifest().AgentDefinitions[0]
			if def.Budget != tc.want {
				t.Fatalf("budget=%+v, want %+v", def.Budget, tc.want)
			}
			var wantTimeout time.Duration
			if tc.budget != nil {
				wantTimeout = tc.budget.Timeout
			}
			if time.Duration(def.Budget.TimeoutMS)*time.Millisecond != wantTimeout {
				t.Fatal("timeout conversion lost precision or overflowed")
			}
			def.Budget = tc.want
			hash, err := wire.AgentDefinitionHash(def)
			if err != nil || hash != h.ContractHash() {
				t.Fatalf("wire budget hash changed: hash=%q err=%v", hash, err)
			}
		})
	}
}

func TestRegisterAgentChildren(t *testing.T) {
	a := New(Config{Description: "Test"})
	taskModel(a)
	leaf := RegisterAgent(a, taskDefinition())
	parent := taskDefinition()
	parent.Slug = "parent"
	parent.Subagents = []Subagent{leaf}
	parent.MaxSubagentCalls = 5
	parent.MaxConcurrentSubagents = 2
	p := RegisterAgent(a, parent)
	if a.buildManifest().AgentDefinitions[0].Budget.Steps != 150 {
		t.Fatal("default budget missing")
	}
	grand := taskDefinition()
	grand.Slug = "grand"
	grand.Subagents = []Subagent{p}
	grand.MaxSubagentCalls = 5
	grand.MaxConcurrentSubagents = 2
	defer func() {
		if recover() == nil {
			t.Fatal("grandchildren accepted")
		}
	}()
	RegisterAgent(a, grand)
}

func TestRegisterAgentReflectedContracts(t *testing.T) {
	type recursive struct {
		Next *recursive `json:"next,omitempty"`
	}
	for _, tc := range []struct {
		name     string
		register func(*Agent)
	}{
		{"recursive", func(a *Agent) { RegisterAgent(a, &AgentDefinition[recursive, taskOutput]{Slug: "worker"}) }},
		{"interface", func(a *Agent) { RegisterAgent(a, &AgentDefinition[any, taskOutput]{Slug: "worker"}) }},
		{"custom encoding", func(a *Agent) { RegisterAgent(a, &AgentDefinition[json.RawMessage, taskOutput]{Slug: "worker"}) }},
		{"bytes", func(a *Agent) { RegisterAgent(a, &AgentDefinition[taskInput, []byte]{Slug: "worker"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{Description: "Test"})
			defer func() {
				if recover() == nil {
					t.Fatal("unsupported reflected contract accepted")
				}
			}()
			tc.register(a)
		})
	}
}

func TestAgentManifestDeterministicOrder(t *testing.T) {
	build := func(slugs []string) wire.AgentManifest {
		a := New(Config{Description: "Test"})
		taskModel(a)
		for _, slug := range slugs {
			d := taskDefinition()
			d.Slug = slug
			RegisterAgent(a, d)
		}
		return a.Manifest()
	}
	if !reflect.DeepEqual(build([]string{"alpha", "beta"}), build([]string{"beta", "alpha"})) {
		t.Fatal("manifest depends on registration order")
	}
}

func TestRegisterAgentExactSchemas(t *testing.T) {
	type input struct {
		Values *[1][2]int8 `json:"values"`
	}
	type output struct {
		Counts [2]uint64 `json:"counts"`
	}
	a := New(Config{Description: "Exact schemas"})
	taskModel(a)
	h := RegisterAgent(a, &AgentDefinition[input, output]{Slug: "worker", Description: "Work", Instructions: "Complete", ModelSlot: "reasoning", MaxAttempts: 1, MaxConcurrency: 1})
	manifest := a.Manifest()
	d := manifest.AgentDefinitions[0]
	if string(d.InputSchema) != string(agentTypeSchema(reflect.TypeFor[input]())) || string(d.OutputSchema) != string(agentTypeSchema(reflect.TypeFor[output]())) {
		t.Fatalf("registered schemas differ: %+v", d)
	}
	hash, err := wire.AgentDefinitionHash(d)
	if err != nil || hash != h.ContractHash() {
		t.Fatalf("hash=%s err=%v", hash, err)
	}
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		t.Fatal(err)
	}
}
