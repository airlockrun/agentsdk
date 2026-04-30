package agentsdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"strings"

	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/stream"
)

// mediaResult is the JS-facing return value for generateImage / speak.
// Path is the absolute storage path the LLM uses for downstream
// printToUser / attachToContext / readBytes calls.
type mediaResult struct {
	Path     string `json:"path"`
	MimeType string `json:"mimeType"`
	Size     int    `json:"size"`
}

// transcribeAudio loads bytes from agent storage at `path`, runs them through
// the system-default transcription model, and returns the goai result. The
// model is resolved server-side by capability — JS callers don't pick a slug.
func (r *run) transcribeAudio(ctx context.Context, path string, opts model.TranscribeCallOptions) (*goai.TranscriptionResult, error) {
	audio, err := r.agent.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}

	if opts.MimeType == "" {
		if info, err := r.agent.StatFile(ctx, path); err == nil && info.ContentType != "" {
			opts.MimeType = info.ContentType
		}
	}

	stt := r.agent.TranscriptionModel(ctx, "", ModelOpts{Capability: CapTranscription})
	return goai.Transcribe(ctx, goai.TranscribeInput{
		Model:           stt,
		Audio:           audio,
		MimeType:        opts.MimeType,
		Filename:        opts.Filename,
		Language:        opts.Language,
		Prompt:          opts.Prompt,
		ProviderOptions: opts.ProviderOptions,
	})
}

// analyzeImage loads bytes from agent storage at `path`, sends them to the
// vision-capability LLM with `question`, and returns the model's reply.
// Capability-routed: airlock picks the agent's vision_model (or system
// default) regardless of which model the agent's main run is using —
// useful when the exec model has no vision support but the platform has
// one configured.
//
// `question` defaults to "Describe this image." when empty.
func (r *run) analyzeImage(ctx context.Context, path, question string) (string, error) {
	imgBytes, err := r.agent.ReadFile(ctx, path)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", path, err)
	}

	mimeType := "image/png"
	if info, err := r.agent.StatFile(ctx, path); err == nil && info.ContentType != "" {
		mimeType = info.ContentType
	}

	if question == "" {
		question = "Describe this image."
	}

	m := r.agent.LLM(ctx, "", ModelOpts{Capability: CapVision})
	res, err := goai.GenerateText(ctx, stream.Input{
		Model: m,
		Messages: []goai.Message{
			message.NewUserMessageWithParts(
				goai.TextPart{Text: question},
				message.ImagePart{
					Image:    base64.StdEncoding.EncodeToString(imgBytes),
					MimeType: mimeType,
				},
			),
		},
	})
	if err != nil {
		return "", err
	}
	// Surface "model returned empty text" as a real error rather than
	// returning an empty string — silent emptiness reads as "the feature
	// is broken" to the caller and we lose the chance to flag a flaky
	// vision call. Caller can retry.
	if strings.TrimSpace(res.Text) == "" {
		return "", fmt.Errorf("vision model returned empty response (finish: %s) — try again or rephrase the question", res.FinishReason)
	}
	return res.Text, nil
}

// generateImage runs the prompt through the system-default image model and
// writes the first generated image to agent storage at `saveAs` (auto-named
// when empty). Returns the path + metadata for downstream printToUser /
// attachToContext calls.
func (r *run) generateImage(ctx context.Context, prompt, saveAs string, opts model.ImageCallOptions) (*mediaResult, error) {
	m := r.agent.ImageModel(ctx, "", ModelOpts{Capability: CapImage})
	res, err := goai.GenerateImage(ctx, goai.ImageInput{
		Model:           m,
		Prompt:          prompt,
		N:               1,
		Size:            opts.Size,
		AspectRatio:     opts.AspectRatio,
		Seed:            opts.Seed,
		ProviderOptions: opts.ProviderOptions,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Images) == 0 {
		return nil, fmt.Errorf("model returned no images")
	}
	img := res.Images[0]
	if img.Base64 == "" {
		return nil, fmt.Errorf("image returned without inline data (URL-only responses not supported)")
	}
	data, err := decodeBase64(img.Base64)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return r.storeMediaResult(ctx, "image", saveAs, data, img.MimeType)
}

// generateSpeech runs `text` through the system-default TTS model and writes
// the audio bytes to agent storage at `saveAs` (auto-named when empty).
func (r *run) generateSpeech(ctx context.Context, text, saveAs string, opts model.SpeechCallOptions) (*mediaResult, error) {
	m := r.agent.SpeechModel(ctx, "", ModelOpts{Capability: CapSpeech})
	res, err := goai.GenerateSpeech(ctx, goai.SpeechInput{
		Model:           m,
		Text:            text,
		Voice:           opts.Voice,
		OutputFormat:    opts.OutputFormat,
		Speed:           opts.Speed,
		ProviderOptions: opts.ProviderOptions,
	})
	if err != nil {
		return nil, err
	}
	if len(res.Audio) == 0 {
		return nil, fmt.Errorf("model returned no audio")
	}
	return r.storeMediaResult(ctx, "speech", saveAs, res.Audio, res.MimeType)
}

// embed proxies the embedding call through Airlock. Texts are small enough
// that the inline-bytes round-trip is fine.
func (r *run) embed(ctx context.Context, texts []string) ([][]float64, error) {
	m := r.agent.EmbeddingModel(ctx, "", ModelOpts{Capability: CapEmbedding})
	res, err := goai.Embed(ctx, goai.EmbedInput{
		Model:  m,
		Values: texts,
	})
	if err != nil {
		return nil, err
	}
	out := make([][]float64, len(res.Embeddings))
	for i, e := range res.Embeddings {
		out[i] = e.Values
	}
	return out, nil
}

// storeMediaResult writes generated bytes to agent storage and returns a
// JS-facing result. When `saveAs` is empty we auto-generate a path under
// the framework-owned /tmp directory (same convention as truncated tool
// outputs in tools.go). When provided, `saveAs` must be an absolute path
// under a registered directory.
func (r *run) storeMediaResult(ctx context.Context, category, saveAs string, data []byte, mimeType string) (*mediaResult, error) {
	if mimeType == "" {
		mimeType = defaultMimeForCategory(category)
	}
	if saveAs == "" {
		saveAs = reservedTmpPath + "/generated/" + category + "-" + randomHex(4) + extForMime(mimeType, category)
	}
	if _, err := r.agent.WriteFile(ctx, saveAs, bytes.NewReader(data), mimeType); err != nil {
		return nil, fmt.Errorf("store %s: %w", saveAs, err)
	}
	canonical, _ := normalizePath(saveAs)
	return &mediaResult{Path: canonical, MimeType: mimeType, Size: len(data)}, nil
}

func defaultMimeForCategory(category string) string {
	switch category {
	case "image":
		return "image/png"
	case "speech":
		return "audio/mpeg"
	}
	return "application/octet-stream"
}

func extForMime(mimeType, category string) string {
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return exts[0]
	}
	switch category {
	case "image":
		return ".png"
	case "speech":
		return ".mp3"
	}
	return ".bin"
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
