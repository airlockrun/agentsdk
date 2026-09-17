package capability

import (
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestCatalog(t *testing.T) {
	if len(Fixed()) != 30 {
		t.Fatalf("fixed inventory = %d, want 30", len(Fixed()))
	}
	manifest := wire.AgentManifest{
		Tools:       []wire.ToolDef{{Name: "output", Access: wire.AccessAdmin, InputSchema: []byte(`{"type":"object"}`)}},
		Connections: []wire.ConnectionDef{{Slug: "mail", Access: wire.AccessUser}},
		Topics:      []wire.TopicDef{{Slug: "alerts", Access: wire.AccessPublic}},
		MCPServers:  []wire.MCPDef{{Slug: "github", Access: wire.AccessAdmin}},
	}
	discovery := Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{
		"github":     {{Name: "search/issues"}, {Name: "search_issues"}},
		"undeclared": {{Name: "execute"}},
	}}
	defs, err := Catalog(manifest, discovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(Fixed())+7 {
		t.Fatalf("inventory count = %d", len(defs))
	}
	for i, d := range defs {
		if i > 0 && defs[i-1].Path.ID() >= d.Path.ID() {
			t.Fatal("catalogue is not deterministic")
		}
		if strings.Contains(d.Path.ID(), "undeclared") {
			t.Fatal("undeclared MCP capability admitted")
		}
		if d.Path.Kind() == Tool && (d.Path.ID() != "tool//output" || d.Access != wire.AccessAdmin || !reflect.DeepEqual(d.InputSchema, manifest.Tools[0].InputSchema)) {
			t.Fatalf("declaration changed: %+v", d)
		}
	}
}

func TestCatalogRejectsConflictsBeforeSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		manifest  wire.AgentManifest
		discovery Discovery
	}{
		{"duplicate tool access", wire.AgentManifest{Tools: []wire.ToolDef{{Name: "check", Access: wire.AccessPublic}, {Name: "check", Access: wire.AccessAdmin}}}, Discovery{}},
		{"invalid access", wire.AgentManifest{Tools: []wire.ToolDef{{Name: "check", Access: "invalid"}}}, Discovery{}},
		{"invalid topic enrollment", wire.AgentManifest{Topics: []wire.TopicDef{{Slug: "alerts", Access: wire.AccessPublic, Enrollment: "invalid"}}}, Discovery{}},
		{"duplicate external name", wire.AgentManifest{MCPServers: []wire.MCPDef{{Slug: "github", Access: wire.AccessUser}}}, Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{"github": {{Name: "search"}, {Name: "search"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Catalog(tc.manifest, tc.discovery); err == nil {
				t.Fatal("conflicting inventory accepted")
			}
		})
	}
}

func TestIdentityDoesNotUsePresentationAliases(t *testing.T) {
	first, err := External(MCP, "canonical", "alias_one", []string{"search/issues"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := External(MCP, "canonical", "alias_two", []string{"search/issues"})
	if err != nil {
		t.Fatal(err)
	}
	if first["search/issues"].ID() != second["search/issues"].ID() || first["search/issues"].Direct() == second["search/issues"].Direct() {
		t.Fatal("identity depends on presentation")
	}
	if first["search/issues"].ID() == Local(MCP, "canonical/search", "issues").ID() {
		t.Fatal("identity path ambiguity")
	}
}

func TestDefinitionCatalogPrivacy(t *testing.T) {
	d := wire.AgentDefinition{Slug: "worker", Description: "Work", Instructions: "Finish", ModelSlot: "reasoning",
		InputSchema: []byte(`{"type":"object"}`), OutputSchema: []byte(`{"type":"object"}`), Budget: wire.AgentBudget{Steps: 150}, MaxAttempts: 1, MaxConcurrency: 1,
		MCPs: []string{"private"}, Tools: []wire.AgentToolDefinition{{Name: "private_tool", Description: "Private", InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`)}}}
	d.ContractHash, _ = wire.AgentDefinitionHash(d)
	m := wire.AgentManifest{AgentDefinitions: []wire.AgentDefinition{d}, ModelSlots: []wire.ModelSlotDef{{Slug: "reasoning", Capability: "text"}},
		Tools: []wire.ToolDef{{Name: "global_tool", Access: wire.AccessUser}}, MCPServers: []wire.MCPDef{{Slug: "private"}, {Slug: "chat", Access: wire.AccessUser}}}
	discovery := Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{"private": {{Name: "lookup"}}, "chat": {{Name: "search"}}}}
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "task"}[scoped], func(t *testing.T) {
			var defs []Definition
			var err error
			if scoped {
				defs, err = DefinitionCatalog(m, wire.RuntimeAgentDefinition{Slug: d.Slug, ContractHash: d.ContractHash}, discovery)
			} else {
				defs, err = Catalog(m, discovery)
			}
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			for _, def := range defs {
				found[def.Path.ID()] = true
			}
			if found["tool//private_tool"] != scoped || found["mcp/private/lookup"] != scoped || found["tool//global_tool"] == scoped || found["mcp/chat/search"] == scoped {
				t.Fatalf("inventory=%v", found)
			}
		})
	}
	if m.MCPServers[0].Access != "" || m.Tools[0].Name != "global_tool" {
		t.Fatal("catalog mutated manifest")
	}
	if _, err := DefinitionCatalog(m, wire.RuntimeAgentDefinition{Slug: d.Slug, ContractHash: "wrong"}, discovery); err == nil {
		t.Fatal("wrong contract accepted")
	}
}
