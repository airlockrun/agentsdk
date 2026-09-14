package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func agentManifest(t *testing.T) AgentManifest {
	t.Helper()
	d := AgentDefinition{Slug: "worker", Description: "Work", Instructions: "Finish the task", ModelSlot: "reasoning",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"string"}`),
		Budget: AgentBudget{Steps: 150}, MaxAttempts: 2, MaxConcurrency: 3}
	d.ContractHash, _ = AgentDefinitionHash(d)
	return AgentManifest{ModelSlots: []ModelSlotDef{{Slug: "reasoning", Capability: "text"}}, AgentDefinitions: []AgentDefinition{d}}
}

func TestAgentDefinitionHash(t *testing.T) {
	d := agentManifest(t).AgentDefinitions[0]
	hash := d.ContractHash
	d.ContractHash = "ignored"
	d.InputSchema = json.RawMessage("{\n  \"type\": \"object\"\n}")
	got, err := AgentDefinitionHash(d)
	if err != nil || got != hash {
		t.Fatalf("hash=%s err=%v", got, err)
	}
	d.Instructions += " carefully"
	got, _ = AgentDefinitionHash(d)
	if got == hash {
		t.Fatal("instructions excluded from contract")
	}
	d.InputSchema = json.RawMessage(`{`)
	if _, err := AgentDefinitionHash(d); err == nil {
		t.Fatal("invalid JSON hashed")
	}
}

func TestValidateAgentDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*AgentManifest)
		valid bool
	}{
		{"valid", func(*AgentManifest) {}, true},
		{"empty manifest", func(m *AgentManifest) { *m = AgentManifest{} }, true},
		{"missing model", func(m *AgentManifest) { m.ModelSlots = nil }, false},
		{"non-text model", func(m *AgentManifest) { m.ModelSlots[0].Capability = "vision" }, false},
		{"zero budget", func(m *AgentManifest) { m.AgentDefinitions[0].Budget = AgentBudget{} }, false},
		{"negative budget", func(m *AgentManifest) { m.AgentDefinitions[0].Budget.Tokens = -1 }, false},
		{"missing attempts", func(m *AgentManifest) { m.AgentDefinitions[0].MaxAttempts = 0 }, false},
		{"missing concurrency", func(m *AgentManifest) { m.AgentDefinitions[0].MaxConcurrency = 0 }, false},
		{"child limits without children", func(m *AgentManifest) { m.AgentDefinitions[0].MaxSubagentCalls = 1 }, false},
		{"unknown MCP", func(m *AgentManifest) { m.AgentDefinitions[0].MCPs = []string{"private"} }, false},
		{"private MCP", func(m *AgentManifest) {
			m.MCPServers = []MCPDef{{Slug: "private"}}
			m.AgentDefinitions[0].MCPs = []string{"private"}
		}, true},
		{"missing schema", func(m *AgentManifest) { m.AgentDefinitions[0].OutputSchema = nil }, false},
		{"null schema", func(m *AgentManifest) { m.AgentDefinitions[0].OutputSchema = json.RawMessage(`null`) }, false},
		{"invalid slug", func(m *AgentManifest) { m.AgentDefinitions[0].Slug = "../worker" }, false},
		{"duplicate", func(m *AgentManifest) { m.AgentDefinitions = append(m.AgentDefinitions, m.AgentDefinitions[0]) }, false},
		{"self child", func(m *AgentManifest) {
			d := &m.AgentDefinitions[0]
			d.Subagents = []string{d.Slug}
			d.MaxSubagentCalls = 1
			d.MaxConcurrentSubagents = 1
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := agentManifest(t)
			tc.edit(&m)
			for i := range m.AgentDefinitions {
				d := &m.AgentDefinitions[i]
				d.ContractHash, _ = AgentDefinitionHash(*d)
			}
			err := ValidateAgentDefinitions(m)
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v valid=%v", err, tc.valid)
			}
		})
	}
	m := agentManifest(t)
	m.AgentDefinitions[0].ContractHash = strings.Repeat("0", 64)
	if err := ValidateAgentDefinitions(m); err == nil {
		t.Fatal("wrong hash accepted")
	}
}

func TestAgentBudgetAndManifestOmission(t *testing.T) {
	raw, _ := json.Marshal(AgentManifest{})
	if strings.Contains(string(raw), "agentDefinitions") {
		t.Fatal("empty definitions not omitted")
	}
	raw, _ = json.Marshal(AgentBudget{Steps: 150})
	if string(raw) != `{"steps":150}` {
		t.Fatalf("budget=%s", raw)
	}
	zero := int64(0)
	raw, _ = json.Marshal(AgentWaitRequest{IDs: []string{"id"}, Mode: "any", TimeoutMS: &zero})
	if !strings.Contains(string(raw), `"timeoutMs":0`) {
		t.Fatal("explicit zero wait timeout omitted")
	}
}
