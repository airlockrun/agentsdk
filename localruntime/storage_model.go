package localruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
)

// storageModel materializes only explicitly configured object references at the
// provider boundary. Transcript/checkpoint data retain their durable references.
type StorageModel struct {
	stream.Model
	Storage  *FileStorage
	Manifest wire.AgentManifest
}

func (m *StorageModel) Stream(ctx context.Context, options *stream.CallOptions) (<-chan stream.Event, error) {
	if options == nil {
		return nil, errors.New("agenttest: model options required")
	}
	copy := *options
	raw, err := json.Marshal(options.Messages)
	if err != nil {
		return nil, err
	}
	copy.Messages = nil
	if err := json.Unmarshal(raw, &copy.Messages); err != nil {
		return nil, err
	}
	var total int64
	for i := range copy.Messages {
		for j, part := range copy.Messages[i].Content.Parts {
			file, ok := part.(message.FilePart)
			if !ok {
				continue
			}
			var reference string
			switch data := file.Data.(type) {
			case message.FileDataBytes:
				reference = data.Data
			case message.FileDataURL:
				reference = data.URL
			default:
				continue
			}
			p, found := strings.CutPrefix(reference, "s3ref:")
			if !found {
				continue
			}
			if m.Storage == nil {
				return nil, errors.New("agenttest: storage attachment requires Options.Storage; canned mock bytes are not image content")
			}
			declared := false
			for _, dir := range m.Manifest.Directories {
				if strings.HasPrefix(p, dir.Path+"/") {
					declared = true
					break
				}
			}
			if !declared {
				return nil, errors.New("agenttest: attachment path is outside registered app directories")
			}
			info, err := m.Storage.Stat(ctx, p)
			if err != nil {
				return nil, fmt.Errorf("agenttest: attachment %q: %w", p, err)
			}
			if file.MimeType != info.ContentType {
				return nil, errors.New("agenttest: attachment MIME type differs from stored object")
			}
			limit := int64(16 << 20)
			if strings.HasPrefix(file.MimeType, "image/") {
				limit = 5 << 20
			}
			if info.Size > limit || total+info.Size > 32<<20 {
				return nil, errors.New("agenttest: attachment byte budget exceeded")
			}
			r, err := m.Storage.Open(ctx, p)
			if err != nil {
				return nil, err
			}
			data, readErr := io.ReadAll(io.LimitReader(r, limit+1))
			closeErr := r.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return nil, err
			}
			if int64(len(data)) != info.Size || int64(len(data)) > limit {
				return nil, errors.New("agenttest: attachment content changed or exceeds limit")
			}
			if strings.HasPrefix(file.MimeType, "image/") {
				_, format, err := image.DecodeConfig(bytes.NewReader(data))
				if err != nil {
					return nil, fmt.Errorf("agenttest: invalid image content: %w", err)
				}
				if "image/"+format != file.MimeType {
					return nil, errors.New("agenttest: image format differs from attachment MIME type")
				}
			}
			total += int64(len(data))
			file.Data = message.FileDataBytes{Data: base64.StdEncoding.EncodeToString(data)}
			copy.Messages[i].Content.Parts[j] = file
		}
	}
	return m.Model.Stream(ctx, &copy)
}
