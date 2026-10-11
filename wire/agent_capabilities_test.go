package wire

import (
	"encoding/json"
	"testing"
)

func TestAgentCapabilityPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *AgentCapabilityPolicy
		valid  bool
	}{
		{"unset", nil, true},
		{"empty", &AgentCapabilityPolicy{}, true},
		{"file", &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityFileRead}}, true},
		{"image", &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityAttachToContext, CapabilityFileStat}}, true},
		{"missing helper dependency", &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityAttachToContext}}, false},
		{"unknown", &AgentCapabilityPolicy{Allow: []AgentCapability{"air//execute"}}, false},
		{"js alias", &AgentCapabilityPolicy{Allow: []AgentCapability{"air.fileRead"}}, false},
		{"direct alias", &AgentCapabilityPolicy{Allow: []AgentCapability{"air__file_read"}}, false},
		{"private declaration owns selection", &AgentCapabilityPolicy{Allow: []AgentCapability{"tool//private"}}, false},
		{"duplicates", &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityFileRead, CapabilityFileRead}}, false},
		{"declared connection", &AgentCapabilityPolicy{Allow: []AgentCapability{"conn/source/request"}}, true},
		{"undeclared connection", &AgentCapabilityPolicy{Allow: []AgentCapability{"conn/other/request"}}, false},
		{"declared topic", &AgentCapabilityPolicy{Allow: []AgentCapability{"topic/events/subscribe"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := agentManifest(t)
			m.Connections = []ConnectionDef{{Slug: "source"}}
			m.Topics = []TopicDef{{Slug: "events"}}
			m.AgentDefinitions[0].Capabilities = tc.policy
			m.AgentDefinitions[0].ContractHash, _ = AgentDefinitionHash(m.AgentDefinitions[0])
			if err := ValidateAgentDefinitions(m); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestAgentCapabilityPolicyContract(t *testing.T) {
	d := agentManifest(t).AgentDefinitions[0]
	unset := d.ContractHash
	d.Capabilities = &AgentCapabilityPolicy{}
	empty, _ := AgentDefinitionHash(d)
	d.Capabilities.Allow = []AgentCapability{CapabilityFileRead}
	read, _ := AgentDefinitionHash(d)
	if unset == empty || empty == read || unset == read {
		t.Fatal("policy distinctions missing from contract hash")
	}
	for _, policy := range []*AgentCapabilityPolicy{nil, {}, {Allow: []AgentCapability{CapabilityFileRead}}} {
		d.Capabilities = policy
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AgentDefinition
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if (decoded.Capabilities == nil) != (policy == nil) {
			t.Fatal("policy presence lost across wire round trip")
		}
	}
}

func TestAgentChildCapabilityCeiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parent, child *AgentCapabilityPolicy
		childMCP      bool
		valid         bool
	}{
		{"unset parent", nil, nil, false, true},
		{"empty child", &AgentCapabilityPolicy{}, &AgentCapabilityPolicy{}, false, true},
		{"unset child escalation", &AgentCapabilityPolicy{}, nil, false, false},
		{"explicit child escalation", &AgentCapabilityPolicy{}, &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityHTTPRequest}}, false, false},
		{"subset", &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityFileRead, CapabilityFileWrite}}, &AgentCapabilityPolicy{Allow: []AgentCapability{CapabilityFileRead}}, false, true},
		{"MCP escalation", &AgentCapabilityPolicy{}, &AgentCapabilityPolicy{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := agentManifest(t)
			parent := &m.AgentDefinitions[0]
			child := *parent
			child.Slug = "child"
			parent.Capabilities, child.Capabilities = tc.parent, tc.child
			parent.Subagents = []string{"child"}
			parent.MaxSubagentCalls, parent.MaxConcurrentSubagents = 1, 1
			if tc.childMCP {
				m.MCPServers = []MCPDef{{Slug: "private"}}
				child.MCPs = []string{"private"}
			}
			parent.ContractHash, _ = AgentDefinitionHash(*parent)
			child.ContractHash, _ = AgentDefinitionHash(child)
			m.AgentDefinitions = append(m.AgentDefinitions, child)
			if err := ValidateAgentDefinitions(m); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}
