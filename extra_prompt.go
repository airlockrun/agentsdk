package agentsdk

// AddExtraPrompt appends a system-prompt fragment that Airlock will include
// for callers whose resolved access on this agent matches one of the
// fragment's Access levels. An empty Access slice means the fragment
// applies to every caller.
//
// Fragments accumulate in registration order and are joined with "\n\n" by
// Airlock at run dispatch, then appended to the sync-rendered system prompt.
// Only /prompt-triggered runs (web + bridge) receive extras — webhook and
// cron handlers run arbitrary Go code and build their own prompts.
func (a *Agent) AddExtraPrompt(p *ExtraPrompt) {
	if p == nil {
		panic("agentsdk: AddExtraPrompt: nil *ExtraPrompt")
	}
	if p.Text == "" {
		panic("agentsdk: AddExtraPrompt: Text is required")
	}
	a.extraPrompts = append(a.extraPrompts, p)
}
