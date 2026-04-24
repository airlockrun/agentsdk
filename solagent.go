package agentsdk

import (
	"fmt"
	"time"

	"github.com/airlockrun/sol/agent"
)

// newSolAgent creates a Sol agent configured for agentsdk execution.
// supportedModalities is the list of input modalities the model supports (e.g. ["text", "image", "pdf"]).
func newSolAgent(a *Agent, run *run, supportedModalities []string) *agent.Agent {
	// Stash modalities on the Run so the lazy-created VM can read them when
	// attachToContext() validates a key's MIME against what the model supports.
	run.supportedModalities = supportedModalities
	return &agent.Agent{
		Name:              "agentsdk",
		Model:             "", // caller sets model or passes Model override
		Tools:             buildSolTools(a, run, supportedModalities),
		MaxSteps:          maxToolSteps,
		SystemPrompt:      a.systemPrompt, // rendered by Airlock during sync
		EnvironmentPrompt: buildEnvironmentPrompt(run),
		HistoryPolicy: agent.HistoryPolicy{
			// Keep attached images/files visible for 3 user turns, then
			// replace with a detach note instructing the LLM to re-attach
			// via attachToContext() if it needs the content back. Strip
			// happens at history-load time; bytes stay in the session
			// store but are swapped for text on every subsequent turn.
			FilesRetainTurns: 3,
		},
	}
}

// buildEnvironmentPrompt creates the per-request environment context.
// Semi-static: changes rarely (date rollover, different platform), so
// LLM provider caching still works for most sequential requests.
func buildEnvironmentPrompt(run *run) string {
	platform := "web"
	if run.bridgeID != "" {
		platform = "bridge"
	}

	env := fmt.Sprintf(`<env>
Date: %s
Platform: %s`, time.Now().Format("2006-01-02"), platform)

	if run.conversationID != "" {
		env += fmt.Sprintf("\nConversation: %s", run.conversationID)
	}

	env += "\n</env>"
	return env
}
