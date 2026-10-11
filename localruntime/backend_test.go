package localruntime

import (
	"encoding/json"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
)

func TestAppBackendRejectsOperationsOutsideDefinitionPolicy(t *testing.T) {
	d := wire.AgentDefinition{Slug: "task", Description: "Task", Instructions: "Complete", ModelSlot: "reasoning", InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`), Budget: wire.AgentBudget{Steps: 10}, MaxAttempts: 1, MaxConcurrency: 1,
		Capabilities: &wire.AgentCapabilityPolicy{Allow: []wire.AgentCapability{wire.CapabilityFileRead}}}
	d.ContractHash, _ = wire.AgentDefinitionHash(d)
	m := wire.AgentManifest{AgentDefinitions: []wire.AgentDefinition{d}, ModelSlots: []wire.ModelSlotDef{{Slug: "reasoning", Capability: "text"}}}
	catalog, err := capability.DefinitionCatalog(m, wire.RuntimeAgentDefinition{Slug: d.Slug, ContractHash: d.ContractHash}, capability.Discovery{})
	if err != nil {
		t.Fatal(err)
	}
	// No HTTP client/URL or platform adapter: denied operations must be rejected
	// by identity selection before consulting any executor or service dependency.
	backend := &AppBackend{Catalog: catalog}
	for _, id := range []string{"air//query_db", "air//http_request", "air//file_write", "air//file_encode", "air//attach_to_context", "air.fileRead", "air__file_read", "tool//run_js"} {
		t.Run(id, func(t *testing.T) {
			_, err := backend.Invoke(t.Context(), chatruntime.Invocation{CapabilityID: id, ToolCallID: "direct", Input: json.RawMessage(`{}`)})
			if err == nil || err.Error() != "localruntime: unknown capability" {
				t.Fatalf("direct dispatch escaped policy: %v", err)
			}
		})
	}
}
