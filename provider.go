package agentsdk

import (
	"context"
	"fmt"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/websearch"
)

// requireSlot returns the declared capability of the registered model slot
// `slug`, panicking if the slug is empty, not registered with RegisterModel,
// or registered with a capability outside `allowed`. Capability is owned by
// the slot declaration — a call site only names the slug — so a missing or
// mismatched registration is a programmer error, surfaced loudly rather than
// silently routed to some default model.
func (a *Agent) requireSlot(slug string, allowed ...ModelCapability) ModelCapability {
	if slug == "" {
		panic("agentsdk: model slug is required — declare the model with RegisterModel and call it by slug")
	}
	for _, s := range a.modelSlots {
		if s.Slug != slug {
			continue
		}
		for _, c := range allowed {
			if s.Capability == c {
				return s.Capability
			}
		}
		panic(fmt.Sprintf("agentsdk: model %q is registered as %q but used here as %v", slug, s.Capability, allowed))
	}
	panic(fmt.Sprintf("agentsdk: model %q is not registered — call RegisterModel(&ModelSlot{Slug: %q, Capability: ...}) before Serve", slug, slug))
}

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

// LLM returns a streaming chat model for the registered slot `slug`. The
// slot's declared capability (CapText or CapVision) selects the model type;
// the operator binds a concrete model to the slot in the Airlock UI, falling
// back to the agent's per-capability default and then the system default.
// Panics if `slug` is empty, not registered with RegisterModel, or registered
// with a non-chat capability. Pass the returned model the same ctx when
// calling Stream.
func (a *Agent) LLM(ctx context.Context, slug string) stream.Model {
	return a.proxyLLM(ctx, slug, a.requireSlot(slug, CapText, CapVision))
}

// ImageModel returns an image generation model for the registered slot `slug`.
// Panics unless `slug` is registered with CapImage.
func (a *Agent) ImageModel(ctx context.Context, slug string) model.ImageModel {
	a.requireSlot(slug, CapImage)
	return a.proxyImage(ctx, slug, CapImage)
}

// EmbeddingModel returns an embedding model for the registered slot `slug`.
// Panics unless `slug` is registered with CapEmbedding.
func (a *Agent) EmbeddingModel(ctx context.Context, slug string) model.EmbeddingModel {
	a.requireSlot(slug, CapEmbedding)
	return a.proxyEmbedding(ctx, slug, CapEmbedding)
}

// SpeechModel returns a text-to-speech model for the registered slot `slug`.
// Panics unless `slug` is registered with CapSpeech.
func (a *Agent) SpeechModel(ctx context.Context, slug string) model.SpeechModel {
	a.requireSlot(slug, CapSpeech)
	return a.proxySpeech(ctx, slug, CapSpeech)
}

// TranscriptionModel returns a speech-to-text model for the registered slot
// `slug`. Panics unless `slug` is registered with CapTranscription.
func (a *Agent) TranscriptionModel(ctx context.Context, slug string) model.TranscriptionModel {
	a.requireSlot(slug, CapTranscription)
	return a.proxyTranscription(ctx, slug, CapTranscription)
}

// The proxy* builders construct a capability-routed model through Airlock.
// They are the internal seam shared by the public slug-based getters above and
// the built-in media tools (vm_media.go), which resolve system-default models
// by capability with an empty slug — a path not exposed to agent authors.
func (a *Agent) proxyLLM(ctx context.Context, slug string, cap ModelCapability) stream.Model {
	r := a.runForCall(ctx) // advance observability accounting + attribute usage
	return proxy.Model("", proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

func (a *Agent) proxyImage(ctx context.Context, slug string, cap ModelCapability) model.ImageModel {
	r := a.runForCall(ctx)
	return proxy.ImageModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

func (a *Agent) proxyEmbedding(ctx context.Context, slug string, cap ModelCapability) model.EmbeddingModel {
	r := a.runForCall(ctx)
	return proxy.EmbeddingModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

func (a *Agent) proxySpeech(ctx context.Context, slug string, cap ModelCapability) model.SpeechModel {
	r := a.runForCall(ctx)
	return proxy.SpeechModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

func (a *Agent) proxyTranscription(ctx context.Context, slug string, cap ModelCapability) model.TranscriptionModel {
	r := a.runForCall(ctx)
	return proxy.TranscriptionModel(proxy.Options{
		BaseURL:    a.client.baseURL,
		Token:      a.client.token,
		Slug:       slug,
		Capability: string(cap),
		Headers:    runIDHeader(r.id),
	})
}

// WebSearch runs a web search through the registered CapSearch slot `slug`. The
// admin binds a search provider to the slot in the Airlock UI; an unbound slot
// resolves to the agent's configured search provider, then the system default —
// the same cascade as an unbound model slot. The call is proxied through Airlock
// (no search API keys in the container). Panics if `slug` is empty or not
// registered with RegisterModel as CapSearch.
//
// Prefer searching directly and feeding the results into GenerateText over
// exposing search as an LLM tool: it's one round-trip instead of a model→
// Airlock→model detour, and the search always runs (a tool the model may
// decline to call doesn't). If you genuinely need the model to decide whether
// to search mid-conversation, wrap this in your own RegisterTool.
func (a *Agent) WebSearch(ctx context.Context, slug string, req websearch.Request) (*websearch.Response, error) {
	a.requireSlot(slug, CapSearch)
	return (&proxySearchClient{client: a.client}).Search(ctx, slug, req)
}
