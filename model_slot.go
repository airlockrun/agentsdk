package agentsdk

// RegisterModel declares a named model slot the agent uses at runtime via
// agent.LLM(ctx, slug) / agent.ImageModel(ctx, slug) / etc. The slot's
// Capability is the single source of truth for the model type — the getters
// take only a slug and read the capability from here. The admin binds a
// concrete model to each slot in the Airlock UI; an unbound slot falls back to
// the agent's per-capability default and then the system default for the
// slot's declared capability. Call before Serve().
//
// Registration is required: every slug passed to a model getter must be
// declared here first. Calling a getter with an unregistered (or empty) slug
// panics — a missing declaration is a programmer error, not a silent
// fall-through to a default model.
func (a *Agent) RegisterModel(slot *ModelSlot) {
	done := a.beginRegistration("RegisterModel")
	defer done()
	if slot == nil {
		panic("agentsdk: RegisterModel: nil *ModelSlot")
	}
	copy := *slot
	validateModelSlot(&copy)
	for _, existing := range a.modelSlots {
		if existing.Slug == copy.Slug {
			panic("agentsdk: duplicate RegisterModel: " + copy.Slug)
		}
	}
	a.modelSlots = append(a.modelSlots, &copy)
}
