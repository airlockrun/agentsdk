package agentsdk

// RegisterModel declares a named model slot the agent will use at runtime
// via agent.LLM(ctx, slug, ...) / agent.ImageModel(...) / etc. The admin
// binds a specific model to each slot in the Airlock UI; if no model is
// bound, calls fall through to the agent's per-capability default and then
// to the system default. Call before Serve().
//
// Registering a slot is optional — undeclared slugs still work, they just
// silently fall through to the capability default and won't appear in the
// admin UI's Models tab.
func (a *Agent) RegisterModel(slug string, opts ModelSlotOpts) {
	if opts.Capability == "" {
		panic("agentsdk: RegisterModel requires a Capability")
	}
	a.modelSlots = append(a.modelSlots, ModelSlotDef{
		Slug:        slug,
		Capability:  string(opts.Capability),
		Description: opts.Description,
	})
}
