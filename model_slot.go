package agentsdk

import "fmt"

// RegisterModel declares a named model slot the agent will use at runtime
// via agent.LLM(ctx, slug, ...) / agent.ImageModel(...) / etc. The admin
// binds a specific model to each slot in the Airlock UI; if no model is
// bound, calls fall through to the agent's per-capability default and then
// to the system default. Call before Serve().
//
// Registering a slot is optional — undeclared slugs still work, they just
// silently fall through to the capability default and won't appear in the
// admin UI's Models tab.
func (a *Agent) RegisterModel(slot *ModelSlot) {
	if slot == nil {
		panic("agentsdk: RegisterModel: nil *ModelSlot")
	}
	if slot.Slug == "" {
		panic("agentsdk: RegisterModel: Slug is required")
	}
	if slot.Capability == "" {
		panic(fmt.Sprintf("agentsdk: RegisterModel(%q): Capability is required", slot.Slug))
	}
	a.modelSlots = append(a.modelSlots, slot)
}
