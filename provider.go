package agentsdk

import (
	"context"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
)

// LLM returns a streaming language model. Capability defaults to CapText
// if ModelOpts.Capability is empty. Pass the returned model the same ctx
// when calling Stream.
func (a *Agent) LLM(ctx context.Context, slug string, opts ModelOpts) stream.Model {
	cap := opts.Capability
	if cap == "" {
		cap = CapText
	}
	_ = a.runForCall(ctx) // advance observability accounting
	return proxy.Model("", proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
	})
}

// ImageModel returns an image generation model proxied through Airlock.
func (a *Agent) ImageModel(ctx context.Context, slug string, opts ModelOpts) model.ImageModel {
	_ = a.runForCall(ctx)
	return proxy.ImageModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapImage),
	})
}

// EmbeddingModel returns an embedding model proxied through Airlock.
func (a *Agent) EmbeddingModel(ctx context.Context, slug string, opts ModelOpts) model.EmbeddingModel {
	_ = a.runForCall(ctx)
	return proxy.EmbeddingModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapEmbedding),
	})
}

// SpeechModel returns a text-to-speech model proxied through Airlock.
func (a *Agent) SpeechModel(ctx context.Context, slug string, opts ModelOpts) model.SpeechModel {
	_ = a.runForCall(ctx)
	return proxy.SpeechModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapSpeech),
	})
}

// TranscriptionModel returns a speech-to-text model proxied through Airlock.
func (a *Agent) TranscriptionModel(ctx context.Context, slug string, opts ModelOpts) model.TranscriptionModel {
	_ = a.runForCall(ctx)
	return proxy.TranscriptionModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(CapTranscription),
	})
}
