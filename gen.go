package agentsdk

import (
	"context"

	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
)

// Generation wrappers: the agent-facing entry points for sub-model calls.
// They mirror the matching goai.* functions but inject the agent run so two
// concerns are handled for the author:
//
//   - Model resolution. When a call leaves the modality's Model unset, the
//     wrapper fills it with the agent's capability-routed proxy model (empty
//     slug → Airlock picks the agent's per-capability default, then the
//     system default). Authors who want a specific registered slot set
//     input.Model = a.LLM(ctx, "slug") (or ImageModel/SpeechModel/…) themselves.
//   - Run attribution + tool reach. Every call resolves a run via runForCall
//     (dispatcher-bound → route-lazy → rolling background), so the wrappers are
//     always callable — including from a detached goroutine with no run in ctx.
//     For text/stream, each tool in input.Tools is wrapped so its Execute runs
//     under that run's context, letting tools touch agent facilities (storage,
//     events, sub-prompts) exactly as a RegisterTool'd tool does.

// GenerateText runs a (multi-step, if input.MaxSteps>1) text generation. Tools
// in input.Tools — typically the same tool.Tool values passed to RegisterTool —
// run under the resolved run, so they can reach agent facilities.
func (a *Agent) GenerateText(ctx context.Context, input stream.Input) (*goai.GenerateTextResult, error) {
	r := a.runForCall(ctx)
	a.prepareGenInput(ctx, &input, r)
	return goai.GenerateText(ctx, input)
}

// StreamText is the streaming counterpart of GenerateText.
func (a *Agent) StreamText(ctx context.Context, input stream.Input) (*stream.Result, error) {
	r := a.runForCall(ctx)
	a.prepareGenInput(ctx, &input, r)
	return goai.StreamText(ctx, input)
}

// prepareGenInput fills a missing chat model and wraps every tool so its
// Execute carries the run. Shared by GenerateText and StreamText.
func (a *Agent) prepareGenInput(ctx context.Context, input *stream.Input, r *run) {
	if input.Model == nil {
		input.Model = a.proxyLLM(ctx, "", CapText)
	}
	if len(input.Tools) == 0 {
		return
	}
	wrapped := make(tool.Set, len(input.Tools))
	for name, t := range input.Tools {
		wrapped[name] = wrapToolWithRun(t, r)
	}
	input.Tools = wrapped
}

// GenerateImage generates an image. A missing Model defaults to the agent's
// image-capability proxy model.
func (a *Agent) GenerateImage(ctx context.Context, input goai.ImageInput) (*goai.ImageResult, error) {
	if input.Model == nil {
		input.Model = a.proxyImage(ctx, "", CapImage)
	}
	return goai.GenerateImage(ctx, input)
}

// GenerateSpeech synthesizes speech. A missing Model defaults to the agent's
// speech-capability proxy model.
func (a *Agent) GenerateSpeech(ctx context.Context, input goai.SpeechInput) (*goai.SpeechResult, error) {
	if input.Model == nil {
		input.Model = a.proxySpeech(ctx, "", CapSpeech)
	}
	return goai.GenerateSpeech(ctx, input)
}

// Transcribe converts speech to text. A missing Model defaults to the agent's
// transcription-capability proxy model.
func (a *Agent) Transcribe(ctx context.Context, input goai.TranscribeInput) (*goai.TranscriptionResult, error) {
	if input.Model == nil {
		input.Model = a.proxyTranscription(ctx, "", CapTranscription)
	}
	return goai.Transcribe(ctx, input)
}

// Embed computes embeddings. A missing Model defaults to the agent's
// embedding-capability proxy model.
func (a *Agent) Embed(ctx context.Context, input goai.EmbedInput) (*goai.EmbedResult, error) {
	if input.Model == nil {
		input.Model = a.proxyEmbedding(ctx, "", CapEmbedding)
	}
	return goai.Embed(ctx, input)
}
