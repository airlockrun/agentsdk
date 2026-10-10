package agenttest

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
)

func storagePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestStorageModelResolvesImageAndRetainsReference(t *testing.T) {
	storage := NewFileStorage(t)
	data := storagePNG(t)
	if _, err := storage.WriteFile(t.Context(), "tmp/image.png", bytes.NewReader(data), "image/png"); err != nil {
		t.Fatal(err)
	}
	mock, err := testutil.NewMockModel(testutil.MockConfig{ID: "storage-model", Default: &testutil.MockResponse{Text: "seen"}})
	if err != nil {
		t.Fatal(err)
	}
	model := &storageModel{Model: mock, storage: storage, manifest: wire.AgentManifest{Directories: []wire.DirectoryDef{{Path: "tmp"}}}}
	opts := &stream.CallOptions{Messages: []message.Message{message.NewUserMessageWithParts(message.FilePart{Data: message.FileDataBytes{Data: "s3ref:tmp/image.png"}, MimeType: "image/png"})}}
	events, err := model.Stream(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	requests := mock.Requests()
	if len(requests) != 1 {
		t.Fatal("no model request")
	}
	file := requests[0].Messages[0].Content.Parts[0].(message.FilePart)
	decoded, err := base64.StdEncoding.DecodeString(file.Data.(message.FileDataBytes).Data)
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatal("actual PNG bytes absent from model file part")
	}
	if opts.Messages[0].Content.Parts[0].(message.FilePart).Data.(message.FileDataBytes).Data != "s3ref:tmp/image.png" {
		t.Fatal("materialization mutated durable reference")
	}
	f, err := storage.OpenFile(t.Context(), "tmp/image.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if !bytes.Equal(got, data) {
		t.Fatal("object mutated")
	}
}

func TestStorageModelFailsBeforeProvider(t *testing.T) {
	for _, tc := range []struct {
		name             string
		storage          bool
		path, mime, data string
	}{
		{"unconfigured", false, "tmp/image.png", "image/png", ""},
		{"traversal", true, "tmp/../secret", "image/png", ""},
		{"undeclared", true, "secret/image.png", "image/png", ""},
		{"missing", true, "tmp/missing.png", "image/png", ""},
		{"malformed_image", true, "tmp/image.png", "image/png", "text is not an image"},
		{"mime_mismatch", true, "tmp/image.png", "image/jpeg", "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := testutil.NewMockModel(testutil.MockConfig{ID: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			model := &storageModel{Model: mock, manifest: wire.AgentManifest{Directories: []wire.DirectoryDef{{Path: "tmp"}}}}
			if tc.storage {
				model.storage = NewFileStorage(t)
				if tc.data != "" {
					if _, err := model.storage.WriteFile(t.Context(), tc.path, strings.NewReader(tc.data), "image/png"); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err = model.Stream(t.Context(), &stream.CallOptions{Messages: []message.Message{message.NewUserMessageWithParts(message.FilePart{Data: message.FileDataBytes{Data: "s3ref:" + tc.path}, MimeType: tc.mime})}})
			if err == nil || len(mock.Requests()) != 0 {
				t.Fatal("invalid storage attachment reached provider")
			}
		})
	}
}
