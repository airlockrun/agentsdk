package agenttest

import (
	"context"
	"github.com/airlockrun/agentsdk/localruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
)

type storageModel struct {
	stream.Model
	storage  *FileStorage
	manifest wire.AgentManifest
}

func (m *storageModel) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	var store *localruntime.FileStorage
	if m.storage != nil {
		store = m.storage.store
	}
	return (&localruntime.StorageModel{Model: m.Model, Storage: store, Manifest: m.manifest}).Stream(ctx, opts)
}
