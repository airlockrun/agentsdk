package agentsdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/sol/websearch"
)

// withBoundRun returns a ctx that carries a run so agent model accessors
// don't trigger background-run creation (which requires a mock for
// /api/agent/run/create — not worth it for unit tests).
func withBoundRun(a *Agent) context.Context {
	return contextWithRun(context.Background(), newRun(a, "run-1", "", "", context.Background()))
}

func TestAgentLLM(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "ocr", Capability: CapVision, Description: "Extract text"})
	ctx := withBoundRun(a)

	m := a.LLM(ctx, "ocr")
	if m == nil {
		t.Fatal("expected non-nil model")
	}

	events, err := m.Stream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	reqs := mock.RequestsByPath("/api/agent/llm/stream")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 llm/stream request, got %d", len(reqs))
	}
	var body struct {
		Slug       string `json:"slug"`
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Slug != "ocr" {
		t.Errorf("slug = %q, want ocr", body.Slug)
	}
	// Capability rides from the slot declaration, not a per-call argument.
	if body.Capability != "vision" {
		t.Errorf("capability = %q, want vision", body.Capability)
	}
}

func TestAgentLLMTextCapability(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "summarize", Capability: CapText})
	ctx := withBoundRun(a)

	m := a.LLM(ctx, "summarize")
	events, err := m.Stream(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	reqs := mock.RequestsByPath("/api/agent/llm/stream")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	var body struct {
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Capability != "text" {
		t.Errorf("capability = %q, want text", body.Capability)
	}
}

func TestAgentLLMPanicsOnUnregisteredSlug(t *testing.T) {
	a, _ := testAgent(t)
	ctx := withBoundRun(a)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unregistered slug")
		}
	}()
	a.LLM(ctx, "never-registered")
}

func TestAgentLLMPanicsOnEmptySlug(t *testing.T) {
	a, _ := testAgent(t)
	ctx := withBoundRun(a)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty slug")
		}
	}()
	a.LLM(ctx, "")
}

func TestAgentLLMPanicsOnCapabilityMismatch(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "poster", Capability: CapImage})
	ctx := withBoundRun(a)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic using an image slot as a chat model")
		}
	}()
	a.LLM(ctx, "poster")
}

func TestAgentWebSearch(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "research", Capability: CapSearch, Description: "Web search"})
	ctx := withBoundRun(a)

	resp, err := a.WebSearch(ctx, "research", websearch.Request{Query: "golang", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected at least one result")
	}

	reqs := mock.RequestsByPath("/api/agent/search")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 search request, got %d", len(reqs))
	}
	var body struct {
		Slug       string `json:"slug"`
		Capability string `json:"capability"`
		Query      string `json:"Query"`
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Slug != "research" {
		t.Errorf("slug = %q, want research", body.Slug)
	}
	if body.Capability != "search" {
		t.Errorf("capability = %q, want search", body.Capability)
	}
	if body.Query != "golang" {
		t.Errorf("query = %q, want golang", body.Query)
	}
}

func TestAgentWebSearchPanicsOnUnregisteredSlug(t *testing.T) {
	a, _ := testAgent(t)
	ctx := withBoundRun(a)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unregistered search slug")
		}
	}()
	_, _ = a.WebSearch(ctx, "never-registered", websearch.Request{Query: "x"})
}

func TestAgentWebSearchPanicsOnCapabilityMismatch(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "summarize", Capability: CapText})
	ctx := withBoundRun(a)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic using a text slot as a search slot")
		}
	}()
	_, _ = a.WebSearch(ctx, "summarize", websearch.Request{Query: "x"})
}

func TestAgentWebSearchTool(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "research", Capability: CapSearch})
	ctx := withBoundRun(a)

	tl := a.WebSearchTool(ctx, "research")
	if tl.Name != "webSearch" {
		t.Errorf("tool name = %q, want webSearch", tl.Name)
	}
}

func TestAgentImageModel(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "render", Capability: CapImage, Description: "Generate chart"})
	ctx := withBoundRun(a)

	m := a.ImageModel(ctx, "render")
	result, err := m.Generate(ctx, model.ImageCallOptions{Prompt: "a chart"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) == 0 {
		t.Fatal("expected at least one image")
	}

	reqs := mock.RequestsByPath("/api/agent/llm/image")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 image request, got %d", len(reqs))
	}
}

func TestAgentEmbeddingModel(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "index", Capability: CapEmbedding})
	ctx := withBoundRun(a)

	m := a.EmbeddingModel(ctx, "index")
	result, err := m.Embed(ctx, model.EmbedCallOptions{Values: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Embeddings) == 0 {
		t.Fatal("expected at least one embedding")
	}

	reqs := mock.RequestsByPath("/api/agent/llm/embedding")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 embedding request, got %d", len(reqs))
	}
}

func TestAgentSpeechModel(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "narrate", Capability: CapSpeech, Description: "Narrate summary"})
	ctx := withBoundRun(a)

	m := a.SpeechModel(ctx, "narrate")
	_, err := m.Generate(ctx, model.SpeechCallOptions{Text: "hello world"})
	if err != nil {
		t.Fatal(err)
	}

	reqs := mock.RequestsByPath("/api/agent/llm/speech")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 speech request, got %d", len(reqs))
	}
}

func TestAgentTranscriptionModel(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterModel(&ModelSlot{Slug: "stt", Capability: CapTranscription})
	ctx := withBoundRun(a)

	m := a.TranscriptionModel(ctx, "stt")
	result, err := m.Transcribe(ctx, model.TranscribeCallOptions{Audio: []byte("fake-audio")})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "mock transcription" {
		t.Errorf("text = %q, want mock transcription", result.Text)
	}

	reqs := mock.RequestsByPath("/api/agent/llm/transcription")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 transcription request, got %d", len(reqs))
	}
}
