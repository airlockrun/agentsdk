package agenttest

import (
	"context"
	"io"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/internal/mockairlock"
)

// FileStorage is SDK test support for a real app-local object namespace. Enable
// it with Options.Storage. Seed only deliberate fixtures through WriteFile;
// source filenames are never resolved as host paths by model callbacks.
type FileStorage struct{ store *mockairlock.FileStorage }

// NewFileStorage creates an isolated t.TempDir namespace and closes it during
// test cleanup. It reads neither credentials nor pre-existing host files.
func NewFileStorage(t *testing.T) *FileStorage {
	t.Helper()
	store, err := mockairlock.NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return &FileStorage{store: store}
}

func (s *FileStorage) WriteFile(ctx context.Context, p string, r io.Reader, mime string) (agentsdk.FileInfo, error) {
	info, err := s.store.Write(ctx, p, r, mime)
	return agentsdk.FileInfo{Path: agentsdk.FilePath(info.Path), Filename: info.Filename, ContentType: info.ContentType, Size: info.Size, LastModified: info.LastModified}, err
}

func (s *FileStorage) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	return s.store.Open(ctx, p)
}
