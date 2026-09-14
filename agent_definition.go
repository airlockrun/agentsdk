package agentsdk

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

// AgentBudget limits a task and its descendants. Zero disables an individual
// limit. Omitting AgentDefinition.Budget selects Steps: 150.
type AgentBudget struct {
	Steps   int64
	Timeout time.Duration // nonnegative, exact whole milliseconds; zero disables the limit
	Tokens  int64
}

// AgentDefinition declares an application-owned task executed by Airlock.
// Tools are private to this definition; they are not registered for app chat.
type AgentDefinition[In, Out any] struct {
	noUnkeyedLiterals
	Slug                   string
	Description            string
	Instructions           string
	ModelSlot              string
	Tools                  []tool.Tool
	MCPs                   []*MCPHandle
	Subagents              []Subagent
	Budget                 *AgentBudget
	MaxAttempts            int
	MaxConcurrency         int
	MaxSubagentCalls       int
	MaxConcurrentSubagents int
}

// Subagent is a sealed reference to an already registered task agent. A child
// must belong to the same app and cannot itself have children.
type Subagent interface{ agentReference() (*Agent, string) }

// AgentHandle binds typed inputs and replies to an immutable task contract.
// Persist run and session IDs to resume observation after a process restart.
type AgentHandle[In, Out any] struct {
	agent *Agent
	slug  string
}

func (h *AgentHandle[In, Out]) agentReference() (*Agent, string) {
	if h == nil {
		panic("agentsdk: nil *AgentHandle")
	}
	return h.agent, h.slug
}

func (h *AgentHandle[In, Out]) Slug() string { return h.registeredAgent().definition.Slug }
func (h *AgentHandle[In, Out]) ContractHash() string {
	return h.registeredAgent().definition.ContractHash
}

type registeredAgent struct {
	definition wire.AgentDefinition
	tools      map[string]tool.Tool
}

func (h *AgentHandle[In, Out]) registeredAgent() *registeredAgent {
	if h == nil || h.agent == nil {
		panic("agentsdk: unbound AgentHandle")
	}
	d := h.agent.agentDefinitions[h.slug]
	if d == nil {
		panic("agentsdk: unbound AgentHandle " + h.slug)
	}
	return d
}

// RegisterAgent snapshots a typed task declaration and validates its references.
// Register its text model, MCP servers and leaf subagents first.
func RegisterAgent[In, Out any](a *Agent, def *AgentDefinition[In, Out]) *AgentHandle[In, Out] {
	if a == nil {
		panic("agentsdk: RegisterAgent: nil *Agent")
	}
	done := a.beginRegistration("RegisterAgent")
	defer done()
	if def == nil {
		panic("agentsdk: RegisterAgent: nil *AgentDefinition")
	}
	if a.agentDefinitions[def.Slug] != nil {
		panic("agentsdk: duplicate RegisterAgent: " + def.Slug)
	}
	declaration := fmt.Sprintf("agentsdk: RegisterAgent(%q)", def.Slug)
	validateContractGoType(declaration, "input", reflect.TypeFor[In](), make(map[reflect.Type]bool))
	validateContractGoType(declaration, "output", reflect.TypeFor[Out](), make(map[reflect.Type]bool))
	for _, n := range []int{def.MaxAttempts, def.MaxConcurrency, def.MaxSubagentCalls, def.MaxConcurrentSubagents} {
		if n < 0 || n > math.MaxInt32 {
			panic("agentsdk: RegisterAgent: limits must fit nonnegative int32")
		}
	}
	d := wire.AgentDefinition{
		Slug: def.Slug, Description: def.Description, Instructions: def.Instructions, ModelSlot: def.ModelSlot,
		InputSchema: agentTypeSchema(reflect.TypeFor[In]()), OutputSchema: agentTypeSchema(reflect.TypeFor[Out]()),
		Budget: wire.AgentBudget{Steps: 150}, MaxAttempts: int32(def.MaxAttempts), MaxConcurrency: int32(def.MaxConcurrency),
		MaxSubagentCalls: int32(def.MaxSubagentCalls), MaxConcurrentSubagents: int32(def.MaxConcurrentSubagents),
	}
	if budget := def.Budget; budget != nil {
		if budget.Timeout < 0 {
			panic(declaration + ": Budget.Timeout must not be negative")
		}
		if budget.Timeout%time.Millisecond != 0 {
			panic(declaration + ": Budget.Timeout must be an exact whole number of milliseconds")
		}
		// Division preserves the full time.Duration range without overflow.
		d.Budget = wire.AgentBudget{Steps: budget.Steps, TimeoutMS: budget.Timeout.Milliseconds(), Tokens: budget.Tokens}
	}
	tools := make(map[string]tool.Tool, len(def.Tools))
	for _, original := range def.Tools {
		t := cloneTool(original)
		if t.Execute == nil || t.IsProviderTool() {
			panic("agentsdk: RegisterAgent: private tool requires native Execute: " + t.Name)
		}
		if _, exists := tools[t.Name]; exists {
			panic("agentsdk: RegisterAgent: duplicate private tool: " + t.Name)
		}
		tools[t.Name] = t
		examples := make([]json.RawMessage, len(t.InputExamples))
		for i, ex := range t.InputExamples {
			examples[i] = append(json.RawMessage(nil), ex.Input...)
		}
		d.Tools = append(d.Tools, wire.AgentToolDefinition{Name: t.Name, Description: t.Description,
			InputSchema: t.InputSchema, OutputSchema: t.OutputSchema, InputExamples: examples})
	}
	sort.Slice(d.Tools, func(i, j int) bool { return d.Tools[i].Name < d.Tools[j].Name })
	for _, m := range def.MCPs {
		if m == nil || m.agent != a || a.mcps[m.slug] == nil {
			panic("agentsdk: RegisterAgent: MCP must be registered on this app")
		}
		d.MCPs = append(d.MCPs, m.slug)
	}
	for _, child := range def.Subagents {
		if child == nil {
			panic("agentsdk: RegisterAgent: nil Subagent")
		}
		owner, slug := child.agentReference()
		if owner != a || a.agentDefinitions[slug] == nil {
			panic("agentsdk: RegisterAgent: subagent must be registered on this app")
		}
		d.Subagents = append(d.Subagents, slug)
	}
	sort.Strings(d.MCPs)
	sort.Strings(d.Subagents)
	hash, err := wire.AgentDefinitionHash(d)
	if err != nil {
		panic("agentsdk: RegisterAgent: " + err.Error())
	}
	d.ContractHash = hash
	manifest := a.buildManifest()
	manifest.AgentDefinitions = append(manifest.AgentDefinitions, d)
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		panic("agentsdk: RegisterAgent: " + err.Error())
	}
	a.agentDefinitions[d.Slug] = &registeredAgent{definition: d, tools: tools}
	return &AgentHandle[In, Out]{agent: a, slug: d.Slug}
}

func (a *Agent) buildAgentDefinitions() []wire.AgentDefinition {
	var defs []wire.AgentDefinition
	for _, slug := range sortedKeys(a.agentDefinitions) {
		// JSON cloning covers every nested schema, example and reference slice.
		raw, err := json.Marshal(a.agentDefinitions[slug].definition)
		if err != nil {
			panic(err)
		}
		var def wire.AgentDefinition
		if err := json.Unmarshal(raw, &def); err != nil {
			panic(err)
		}
		defs = append(defs, def)
	}
	return defs
}
