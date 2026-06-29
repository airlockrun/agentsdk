package agentsdk

import "github.com/airlockrun/goai/tool"

// registeredTool is the internal representation stored on *Agent: a goai
// tool.Tool (the single tool currency — name, description, input/output schema,
// examples, execute) plus the agentsdk-only concerns goai's provider-agnostic
// tool.Tool does not model: access gating and a model-only LLM hint.
//
// The embedded tool.Tool promotes Name/Description/InputSchema/OutputSchema/
// InputExamples/Execute, so the same value an author builds with
// tool.Typed[In,Out] flows through RegisterTool, the system prompt, sync, and
// the agent's GenerateText/StreamText sub-calls unchanged.
type registeredTool struct {
	tool.Tool
	access  Access
	llmHint string
}
