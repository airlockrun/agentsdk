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
		SystemPrompt:      a.systemPromptSnapshot(), // rendered by Airlock during sync; mutex-guarded so /refresh updates are visible
		EnvironmentPrompt: buildEnvironmentPrompt(run),
		HistoryPolicy: agent.HistoryPolicy{
			// FilesRetainTurns=0 keeps every attached image/file in history
			// across the whole conversation. The earlier 3-turn window was
			// cache-hostile: the strip boundary moved every turn, mutating
			// older messages and invalidating provider prompt cache from the
			// first changed message onward. Pairs with attachref's URL cache
			// (attachment_url_cache table) which keeps the presigned URL
			// string stable across turns — without that, presigned-URL
			// rotation would invalidate cache at every image part anyway.
			//
			// Token cost grows linearly with image-heavy convos
			// (~1500 tokens/image, ~1000 tokens/file estimated). Eventual
			// fail-safes: attachref's per-request inline cap evicts oldest
			// to placeholders, and sol's session.Prune trims on overflow.
			FilesRetainTurns: 0,
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
