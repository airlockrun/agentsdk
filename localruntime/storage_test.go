package localruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testFileStorage(t *testing.T) *FileStorage {
	t.Helper()
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFileStorageObjects(t *testing.T) {
	s := testFileStorage(t)
	data := []byte{0, 255, 13, 10, 0, 1}
	info, err := s.Write(t.Context(), "uploads/binary", bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(data)) || info.Filename != "binary" || info.LastModified.IsZero() {
		t.Fatalf("bad metadata: %+v", info)
	}
	f, err := s.Open(t.Context(), "uploads/binary")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("binary roundtrip: %v", err)
	}
	if err := s.Copy(t.Context(), "uploads/binary", "uploads/nested/copy"); err != nil {
		t.Fatal(err)
	}
	files, err := s.List(t.Context(), "uploads", false)
	if err != nil || len(files) != 1 {
		t.Fatalf("flat list: %+v %v", files, err)
	}
	files, err = s.List(t.Context(), "uploads", true)
	if err != nil || len(files) != 2 {
		t.Fatalf("recursive list: %+v %v", files, err)
	}
	if err := s.Delete(t.Context(), "uploads/binary"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), "uploads/binary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(t.Context(), "uploads/binary"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestFileStorageBoundary(t *testing.T) {
	s := testFileStorage(t)
	for _, p := range []string{"../outside", "tmp/../../outside", "/tmp/outside", "tmp//x", "tmp/./x", "tmp/a\\b", ".", "tmp/\x00", "tmp/new\nline", "tmp/" + string([]byte{255})} {
		t.Run(p, func(t *testing.T) {
			if _, err := s.Write(t.Context(), p, strings.NewReader("bad"), "text/plain"); err == nil {
				t.Fatal("invalid path accepted")
			}
			if _, err := s.Open(t.Context(), p); err == nil {
				t.Fatal("invalid read accepted")
			}
		})
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not an object"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Symlink(outside, "escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(t.Context(), "escape/write", strings.NewReader("bad"), "text/plain"); err == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, err := s.Open(t.Context(), "escape/secret"); err == nil {
		t.Fatal("symlink read accepted")
	}
	if err := s.root.Mkdir("inside", 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Symlink("inside", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(t.Context(), "alias/write", strings.NewReader("bad"), "text/plain"); err == nil {
		t.Fatal("object-store symlink alias accepted")
	}
}

func TestFileStorageFailedWritesAndClose(t *testing.T) {
	s := testFileStorage(t)
	if _, err := s.Write(t.Context(), "tmp/a", strings.NewReader("original"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(t.Context(), "tmp/a", io.LimitReader(zeroStorageReader{}, maxStorageObject+1), "text/plain"); err == nil {
		t.Fatal("oversized object accepted")
	}
	f, err := s.Open(t.Context(), "tmp/a")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "original" {
		t.Fatal("failed write changed existing object")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Write(ctx, "tmp/cancelled", strings.NewReader("bad"), "text/plain"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			t.Fatal("staging file leaked")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), "tmp/a"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed storage: %v", err)
	}
}

func TestMockStorageHTTP(t *testing.T) {
	s := testFileStorage(t)
	mux := http.NewServeMux()
	mux.Handle("GET /api/agent/storage/{key...}", StorageHandler(s, "get"))
	server := httptest.NewServer(mux)
	defer server.Close()
	url := server.URL
	if _, err := s.Write(t.Context(), "tmp/a", strings.NewReader("abcdef"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", url+"/api/agent/storage/tmp/a", nil)
	req.Header.Set("Range", "bytes=1-3")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 206 || string(data) != "bcd" || response.Header.Get("Content-Range") != "bytes 1-3/6" {
		t.Fatalf("range status=%d body=%q", response.StatusCode, data)
	}
	response, err = http.Get(url + "/api/agent/storage/tmp/missing")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("configured storage fell back to canned content")
	}
}

type zeroStorageReader struct{}

func (zeroStorageReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
