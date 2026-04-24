package agentsdk

// AddExtraPrompt appends a system-prompt fragment that Airlock will include
// for callers whose resolved access on this agent is in the supplied set.
// Passing no access levels means the fragment applies to every caller.
//
// Fragments accumulate in registration order and are joined with "\n\n" by
// Airlock at run dispatch, then appended to the sync-rendered system prompt.
// Only /prompt-triggered runs (web + bridge) receive extras — webhook and
// cron handlers run arbitrary Go code and build their own prompts.
func (a *Agent) AddExtraPrompt(prompt string, access ...Access) {
	a.extraPrompts = append(a.extraPrompts, ExtraPromptSpec{
		Text:   prompt,
		Access: access,
	})
}
