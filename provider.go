package agentsdk

import (
	"context"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
)

// runIDHeader returns the attribution header airlock uses to tie a proxied
// model call to its originating run for token/cost accounting. Empty run ID
// yields nil so no blank header is sent (airlock then records the call
// unattributed rather than mis-attributed).
func runIDHeader(runID string) map[string]string {
	if runID == "" {
		return nil
	}
	return map[string]string{"X-Airlock-Run-ID": runID}
}

// LLM returns a streaming language model. Capability defaults to CapText
// if ModelOpts.Capability is empty. Pass the returned model the same ctx
// when calling Stream.
func (a *Agent) LLM(ctx context.Context, slug string, opts ModelOpts) stream.Model {
	cap := opts.Capability
	if cap == "" {
		cap = CapText
	}
	r := a.runForCall(ctx) // advance observability accounting + attribute usage
	return proxy.Model("", proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

// ImageModel returns an image generation model proxied through Airlock.
func (a *Agent) ImageModel(ctx context.Context, slug string, opts ModelOpts) model.ImageModel {
	r := a.runForCall(ctx)
	return proxy.ImageModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapImage),
		Headers:    runIDHeader(r.id),
	})
}

// EmbeddingModel returns an embedding model proxied through Airlock.
func (a *Agent) EmbeddingModel(ctx context.Context, slug string, opts ModelOpts) model.EmbeddingModel {
	r := a.runForCall(ctx)
	return proxy.EmbeddingModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapEmbedding),
		Headers:    runIDHeader(r.id),
	})
}

// SpeechModel returns a text-to-speech model proxied through Airlock.
func (a *Agent) SpeechModel(ctx context.Context, slug string, opts ModelOpts) model.SpeechModel {
	r := a.runForCall(ctx)
	return proxy.SpeechModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapSpeech),
		Headers:    runIDHeader(r.id),
	})
}

// TranscriptionModel returns a speech-to-text model proxied through Airlock.
func (a *Agent) TranscriptionModel(ctx context.Context, slug string, opts ModelOpts) model.TranscriptionModel {
	r := a.runForCall(ctx)
	return proxy.TranscriptionModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapTranscription),
		Headers:    runIDHeader(r.id),
	})
}
