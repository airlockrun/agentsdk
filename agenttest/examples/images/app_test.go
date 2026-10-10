package imagesexample_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
)

var models = agenttest.RegisterFlags(flag.CommandLine)

func TestStoredImage(t *testing.T) {
	t.Chdir(filepath.Join("testdata", "app"))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if x >= 32 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	hash := sha256.Sum256(data)
	storage := agenttest.NewFileStorage(t)
	if _, err := storage.WriteFile(ctx, "evidence/probe.png", bytes.NewReader(data), "image/png"); err != nil {
		t.Fatal(err)
	}
	options := *models
	options.Storage = storage
	live := options.Models["image_probe"] != ""
	var audit *imageTransport
	if live {
		base := http.DefaultClient
		transport := base.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		audit = &imageTransport{base: transport, want: hash}
		client := *base
		client.Transport = audit
		http.DefaultClient = &client
		t.Cleanup(func() { http.DefaultClient = base })
	}
	type Output struct {
		Description string `json:"description"`
	}
	var handle *agentsdk.AgentHandle[struct{}, Output]
	env := agenttest.NewWithOptions(t, func() *agentsdk.Agent {
		app := agentsdk.New(agentsdk.Config{Description: "Stored image example"})
		app.RegisterDirectory("evidence", agentsdk.DirectoryOpts{Read: agentsdk.AccessInternal, Write: agentsdk.AccessInternal, List: agentsdk.AccessInternal, Description: "Task image evidence"})
		app.RegisterModel(&agentsdk.ModelSlot{Slug: "image_probe", Capability: agentsdk.CapText, Description: "Inspect stored image"})
		handle = agentsdk.RegisterAgent(app, &agentsdk.AgentDefinition[struct{}, Output]{Slug: "image_probe", ModelSlot: "image_probe", Description: "Describe a stored image", Instructions: "Use run_js to attach evidence/probe.png with air.attachToContext. Then call complete with an output description of the visible image. Do not use any other capability.", Budget: &agentsdk.AgentBudget{Steps: 4, Timeout: time.Minute, Tokens: 30000}, MaxAttempts: 1, MaxConcurrency: 1})
		return app
	}, options)
	mock := env.MockModel("image_probe")
	if err := mock.Configure(testutil.MockConfig{ID: mock.ID(), Responses: []testutil.MockResponse{
		{ToolCalls: []stream.ToolCall{{ID: "attach", Name: "run_js", Input: json.RawMessage(`{"code":"await air.attachToContext({path:'evidence/probe.png'}); return {attached:true};","description":"Inspect stored image"}`)}}, Usage: stream.UsageFrom(10, 5)},
		{ToolCalls: []stream.ToolCall{{ID: "finish", Name: "complete", Input: json.RawMessage(`{"kind":"output","output":{"description":"Image inspected"}}`)}}, Usage: stream.UsageFrom(10, 5)},
	}}); err != nil {
		t.Fatal(err)
	}
	limits := jsexec.DefaultLimits()
	var config agenttest.ExecutorConfig
	if os.Getenv("AIRLOCK_TEST_EXECUTOR_URL") != "" || os.Getenv("AIRLOCK_TEST_EXECUTOR_TOKEN") != "" {
		var err error
		config, err = agenttest.ExecutorConfigFromEnv(limits)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		image := os.Getenv("TEST_JSEXEC_IMAGE")
		if image == "" {
			image = "agentsdk-images-example:local"
			if err := jsexec.BuildImage(ctx, image); err != nil {
				t.Fatal(err)
			}
		}
		config = agenttest.ExecutorConfig{Image: image, Limits: limits}
	}
	factory, err := agenttest.Executor(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agenttest.RunAgent(t, ctx, env, handle, struct{}{}, agenttest.RunOptions{ExecutorFactory: factory, MaxSteps: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply == nil || result.Reply.Output == nil || result.Reply.Output.Description == "" {
		t.Fatal("missing image description")
	}
	if result.Runtime.CleanupError != nil {
		t.Fatal(result.Runtime.CleanupError)
	}
	if live {
		audit.mu.Lock()
		seen := audit.seen
		audit.mu.Unlock()
		if !seen {
			for _, call := range result.AppCalls {
				if call.Error != "" {
					t.Logf("app callback error: %s", call.Error)
				}
			}
			t.Fatalf("exact PNG bytes absent from real provider input_image; description=%q; callbacks=%d", result.Reply.Output.Description, len(result.Calls))
		}
		t.Logf("Exact seeded PNG reached provider input_image; steps=%d tokens=%d; %s", result.Steps, result.Tokens, result.Reply.Output.Description)
	} else {
		seen := false
		for _, request := range mock.Requests() {
			for _, msg := range request.Messages {
				for _, part := range msg.Content.Parts {
					if file, ok := part.(message.FilePart); ok {
						if file.MimeType == "image/png" {
							encoded, ok := file.Data.(message.FileDataBytes)
							if !ok {
								t.Fatal("image was not materialized")
							}
							decoded, err := base64.StdEncoding.DecodeString(encoded.Data)
							if err != nil || sha256.Sum256(decoded) != hash {
								t.Fatal("image content differs")
							}
							seen = true
						}
					}
				}
			}
		}
		if !seen {
			t.Fatal("mock provider did not receive native image content")
		}
	}
	// Checkpoints must retain references, not provider materialized image bytes.
	for _, msg := range result.Messages {
		for _, part := range msg.Parts {
			if part.File != nil && part.File.MimeType == "image/png" && part.File.Data != "s3ref:evidence/probe.png" {
				t.Fatal("checkpoint contains inline image data")
			}
		}
	}
	// SDK stat/list/range/copy/delete use the same configured mock-host objects.
	info, err := env.Agent.StatFile(ctx, "evidence/probe.png")
	if err != nil || info.Size != int64(len(data)) {
		t.Fatalf("stat: %+v %v", info, err)
	}
	chunk, err := env.Agent.ReadRange(ctx, "evidence/probe.png", 0, 7)
	if err != nil || !bytes.Equal(chunk, data[:8]) {
		t.Fatalf("range: %v", err)
	}
	if err := env.Agent.CopyFile(ctx, "evidence/probe.png", "evidence/copy.png"); err != nil {
		t.Fatal(err)
	}
	files, err := env.Agent.ListDir(ctx, "evidence", agentsdk.ListOpts{})
	if err != nil || len(files) != 2 {
		t.Fatalf("list: %+v %v", files, err)
	}
	if err := env.Agent.DeleteFile(ctx, "evidence/copy.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Agent.ReadFile(ctx, "evidence/missing.png"); err != agentsdk.ErrNotFound {
		t.Fatalf("missing file must not use canned storage: %v", err)
	}
}

// Only image content hashes are retained. No auth header, prompt, credential or
// complete request/response body is logged or written to a fixture artifact.
type imageTransport struct {
	base http.RoundTripper
	want [32]byte
	mu   sync.Mutex
	seen bool
}

func (a *imageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.URL.Path == "/backend-api/codex/responses" {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(data))
		var body any
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
		var walk func(any) error
		walk = func(v any) error {
			switch x := v.(type) {
			case map[string]any:
				if x["type"] == "input_image" {
					url, _ := x["image_url"].(string)
					const prefix = "data:image/png;base64,"
					if len(url) >= len(prefix) && url[:len(prefix)] == prefix {
						png, err := base64.StdEncoding.DecodeString(url[len(prefix):])
						if err != nil {
							return err
						}
						if sha256.Sum256(png) == a.want {
							a.mu.Lock()
							a.seen = true
							a.mu.Unlock()
						}
					}
				}
				for _, child := range x {
					if err := walk(child); err != nil {
						return err
					}
				}
			case []any:
				for _, child := range x {
					if err := walk(child); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(body); err != nil {
			return nil, err
		}
	}
	return a.base.RoundTrip(req)
}
