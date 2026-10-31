package agentsdk

import "github.com/airlockrun/agentsdk/wire"

// SuggestedPrompt declares a useful app-specific workflow as editable chat copy.
type SuggestedPrompt = wire.SuggestedPrompt

// RegisterSuggestedPrompt adds static composer copy in display order. Register
// 1–3 meaningful workflows supported by the app. Invalid declarations panic.
// Selecting a suggestion never sends it or grants permissions.
func (a *Agent) RegisterSuggestedPrompt(prompt SuggestedPrompt) {
	done := a.beginRegistration("RegisterSuggestedPrompt")
	defer done()
	prompts := append(append([]wire.SuggestedPrompt(nil), a.suggestedPrompts...), prompt)
	if err := wire.ValidateSuggestedPrompts(prompts); err != nil {
		panic("agentsdk: RegisterSuggestedPrompt: " + err.Error())
	}
	a.suggestedPrompts = prompts
}

func (a *Agent) suggestedPromptManifest() []wire.SuggestedPrompt {
	return append([]wire.SuggestedPrompt(nil), a.suggestedPrompts...)
}
