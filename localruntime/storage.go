package localruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/gofrs/flock"
)

const maxStorageObject = 80 << 20

// FileStorage is a durable app-local namespace. An atomically published catalog
// maps logical paths to immutable SHA-256 blobs. File locks serialize replicas;
// every operation reloads the catalog from shared disk.
type FileStorage struct {
	mu     sync.Mutex
	root   *os.Root
	files  map[string]wire.FileInfo
	hashes map[string]string
	lock   *flock.Flock
	closed bool
}

func NewFileStorage(root string) (*FileStorage, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	if err := r.MkdirAll(".objects", 0700); err != nil {
		r.Close()
		return nil, err
	}
	return &FileStorage{root: r, lock: flock.New(filepath.Join(root, ".catalog.lock"))}, nil
}

func (s *FileStorage) acquire(ctx context.Context) error {
	if s.closed {
		return os.ErrClosed
	}
	ok, err := s.lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	s.files, s.hashes = map[string]wire.FileInfo{}, map[string]string{}
	raw, err := s.root.ReadFile(".catalog.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		var catalog struct {
			Files  map[string]wire.FileInfo
			Hashes map[string]string
		}
		err = json.Unmarshal(raw, &catalog)
		if err == nil && (catalog.Files == nil || catalog.Hashes == nil) {
			err = errors.New("localruntime: invalid storage catalog")
		}
		if err == nil {
			s.files, s.hashes = catalog.Files, catalog.Hashes
		}
	}
	if err != nil {
		s.lock.Unlock()
	}
	return err
}

func (s *FileStorage) persist() error {
	raw, err := json.Marshal(struct {
		Files  map[string]wire.FileInfo
		Hashes map[string]string
	}{s.files, s.hashes})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.root.Name(), ".catalog-")
	if err != nil {
		return err
	}
	name := path.Base(f.Name())
	defer s.root.Remove(name)
	_, err = f.Write(raw)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := s.root.Rename(name, ".catalog.json"); err != nil {
		return err
	}
	d, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func storagePath(value string, directory bool) error {
	if directory && value == "" {
		return nil
	}
	if value == "" || path.Clean(value) != value || strings.HasPrefix(value, "/") || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsAny(value, "\\\x00") {
		return errors.New("mockairlock: canonical app-relative storage path required")
	}
	if strings.HasPrefix(value, ".") {
		return errors.New("localruntime: reserved storage path")
	}
	if !utf8.ValidString(value) {
		return errors.New("mockairlock: storage path must be valid UTF-8")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("mockairlock: storage path contains a control character")
		}
	}
	return nil
}

func (s *FileStorage) check(ctx context.Context, p string, directory bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return os.ErrClosed
	}
	if err := storagePath(p, directory); err != nil {
		return err
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		if p == "" {
			break
		}
		info, err := s.root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("mockairlock: symlinks are not storage objects")
		}
	}
	return nil
}

func (s *FileStorage) Write(ctx context.Context, p string, r io.Reader, mime string) (wire.FileInfo, error) {
	if r == nil {
		return wire.FileInfo{}, errors.New("mockairlock: object reader required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return wire.FileInfo{}, err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, p, false); err != nil {
		return wire.FileInfo{}, err
	}
	if err := s.root.MkdirAll(path.Dir(p), 0700); err != nil {
		return wire.FileInfo{}, err
	}
	f, err := os.CreateTemp(s.root.Name(), ".upload-")
	if err != nil {
		return wire.FileInfo{}, err
	}
	staging := path.Base(f.Name())
	defer s.root.Remove(staging)
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(storageReader{ctx, r}, maxStorageObject+1))
	closeErr := errors.Join(f.Sync(), f.Close())
	if err := errors.Join(copyErr, closeErr, ctx.Err()); err != nil {
		return wire.FileInfo{}, err
	}
	if n > maxStorageObject {
		return wire.FileInfo{}, errors.New("mockairlock: object exceeds 80 MiB")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if err := s.root.Rename(staging, ".objects/"+digest); err != nil {
		return wire.FileInfo{}, err
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	info := wire.FileInfo{Path: p, Filename: path.Base(p), ContentType: mime, Size: n, LastModified: time.Now().UTC()}
	s.files[p] = info
	s.hashes[p] = digest
	return info, s.persist()
}

func (s *FileStorage) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, p, false); err != nil {
		return nil, err
	}
	if _, ok := s.files[p]; !ok {
		return nil, os.ErrNotExist
	}
	f, err := s.root.Open(".objects/" + s.hashes[p])
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("mockairlock: regular object required")
	}
	return &storageReadCloser{storageReader{ctx, f}, f}, nil
}

func (s *FileStorage) Stat(ctx context.Context, p string) (wire.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return wire.FileInfo{}, err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, p, false); err != nil {
		return wire.FileInfo{}, err
	}
	info, ok := s.files[p]
	if !ok {
		return info, os.ErrNotExist
	}
	return info, nil
}

func (s *FileStorage) List(ctx context.Context, p string, recursive bool) ([]wire.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, p, true); err != nil {
		return nil, err
	}
	files := []wire.FileInfo{}
	for name, info := range s.files {
		rest := name
		if p != "" {
			var found bool
			rest, found = strings.CutPrefix(name, p+"/")
			if !found {
				continue
			}
		}
		if !recursive && strings.Contains(rest, "/") {
			continue
		}
		if err := s.check(ctx, name, false); err != nil {
			return nil, err
		}
		files = append(files, info)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (s *FileStorage) Delete(ctx context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, p, false); err != nil {
		return err
	}
	delete(s.files, p)
	delete(s.hashes, p)
	return s.persist()
}

func (s *FileStorage) Copy(ctx context.Context, src, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.lock.Unlock()
	if err := s.check(ctx, src, false); err != nil {
		return err
	}
	if err := s.check(ctx, dst, false); err != nil {
		return err
	}
	info, ok := s.files[src]
	if !ok {
		return os.ErrNotExist
	}
	info.Path = dst
	info.Filename = path.Base(dst)
	info.LastModified = time.Now().UTC()
	s.files[dst] = info
	s.hashes[dst] = s.hashes[src]
	return s.persist()
}

func (s *FileStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}

type storageReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r storageReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type storageReadCloser struct {
	storageReader
	closer io.Closer
}

func (r *storageReadCloser) Close() error { return r.closer.Close() }

func storageError(err error) error { return fmt.Errorf("mockairlock: storage: %w", err) }
