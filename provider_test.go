package agentsdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/airlockrun/goai/model"
)

// withBoundRun returns a ctx that carries a run so agent model accessors
// don't trigger background-run creation (which requires a mock for
// /api/agent/run/create — not worth it for unit tests).
func withBoundRun(a *Agent) context.Context {
	return contextWithRun(context.Background(), newRun(a, "run-1", "", "", context.Background()))
}

func TestAgentLLM(t *testing.T) {
	a, mock := testAgent(t)
	ctx := withBoundRun(a)

	m := a.LLM(ctx, "ocr", ModelDef{Capability: CapVision, Description: "Extract text"})
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
	if body.Capability != "vision" {
		t.Errorf("capability = %q, want vision", body.Capability)
	}
}

func TestAgentLLMDefaultCapability(t *testing.T) {
	a, mock := testAgent(t)
	ctx := withBoundRun(a)

	m := a.LLM(ctx, "summarize", ModelDef{})
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
		t.Errorf("capability = %q, want text (default)", body.Capability)
	}
}

func TestAgentImageModel(t *testing.T) {
	a, mock := testAgent(t)
	ctx := withBoundRun(a)

	m := a.ImageModel(ctx, "render", ModelDef{Description: "Generate chart"})
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
	ctx := withBoundRun(a)

	m := a.EmbeddingModel(ctx, "index", ModelDef{})
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
	ctx := withBoundRun(a)

	m := a.SpeechModel(ctx, "narrate", ModelDef{Description: "Narrate summary"})
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
	ctx := withBoundRun(a)

	m := a.TranscriptionModel(ctx, "stt", ModelDef{})
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
