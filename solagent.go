package agentsdk

import (
	"time"

	"github.com/airlockrun/sol/agent"
)

// newSolAgent creates a Sol agent configured for agentsdk execution.
func newSolAgent(a *Agent, run *run) *agent.Agent {
	// Stash the model's input modalities on the Run so the lazy-created VM can
	// read them when attachToContext() validates a key's MIME against what the
	// model supports. Sourced from the synced PromptData, which airlock keeps
	// current across model changes (immediate /refresh on update + a
	// dispatch-time hash check that self-heals a stale cache before the run).
	a.syncMu.RLock()
	supportedModalities := a.promptData.SupportedModalities
	a.syncMu.RUnlock()
	run.supportedModalities = supportedModalities
	env := promptEnv{
		Date:         time.Now().Format("2006-01-02"),
		Platform:     run.platform,
		UserName:     run.userDisplayName,
		UserEmail:    run.userEmail,
		Conversation: run.conversationID,
	}
	return &agent.Agent{
		Name:         "agentsdk",
		Model:        "", // caller sets model or passes Model override
		Tools:        buildSolTools(a, run),
		MaxSteps:     maxToolSteps,
		SystemPrompt: a.renderSystemPrompt(run.callerAccess, run.visibleSiblings, env, run.directTools), // rendered per-run from live registrations + synced PromptData; the <env> block carries per-turn date/platform/user/conversation
		// Redactor closes over a's live sensitive set so values
		// registered after Run start (via secret.Get inside a tool) are
		// stripped from the next-step LLM input.
		Redactor: a.redactSensitive,
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
