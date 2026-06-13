package agentsdk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func readAllClose(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}

// storageMock is an in-memory agent-storage backend with byte-range support
// and per-path GET counting, so cache tests can prove a second read is served
// from disk (no second backend GET).
type storageMock struct {
	mu       sync.Mutex
	files    map[string][]byte
	getCount map[string]int
	server   *httptest.Server
}

func newStorageMock() *storageMock {
	s := &storageMock{files: map[string][]byte{}, getCount: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		var buf bytes.Buffer
		buf.ReadFrom(r.Body)
		s.mu.Lock()
		s.files[key] = buf.Bytes()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		s.mu.Lock()
		delete(s.files, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		s.mu.Lock()
		b, ok := s.files[key]
		s.getCount[key]++
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if rh := r.Header.Get("Range"); rh != "" {
			start, end, valid := parseTestRange(rh, int64(len(b)))
			if !valid {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(b)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(b)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(b[start : end+1])
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("POST /api/agent/storage/info", func(w http.ResponseWriter, r *http.Request) {
		dec := bytes.Buffer{}
		dec.ReadFrom(r.Body)
		// The request body is {"path":"..."}.
		path := extractJSONPath(dec.String())
		s.mu.Lock()
		b, ok := s.files[path]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"filename":%q,"size":%d,"contentType":"application/octet-stream"}`, path, path, len(b))
	})
	s.server = httptest.NewServer(mux)
	return s
}

func parseTestRange(h string, size int64) (int64, int64, bool) {
	spec := strings.TrimPrefix(h, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(spec[dash+1:], 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

func extractJSONPath(s string) string {
	const k = `"path":"`
	i := strings.Index(s, k)
	if i < 0 {
		return ""
	}
	rest := s[i+len(k):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (s *storageMock) gets(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCount[key]
}

func (s *storageMock) put(key string, b []byte) {
	s.mu.Lock()
	s.files[key] = b
	s.mu.Unlock()
}

// storageAgent builds an Agent pointed at a fresh storageMock plus a run to
// drive the cache-aware methods.
func storageAgent(t *testing.T) (*Agent, *storageMock, *run) {
	t.Helper()
	mock := newStorageMock()
	t.Cleanup(func() { mock.server.Close() })
	a := &Agent{
		agentID:    "test-agent",
		apiURL:     mock.server.URL,
		token:      "test-token",
		httpClient: &http.Client{},
	}
	a.directories = append(a.directories, &Directory{
		Path: reservedTmpPath, Read: AccessUser, Write: AccessUser, List: AccessUser,
	})
	a.client = newAirlockClient(mock.server.URL, "test-token", a.httpClient)
	r := newRun(a, "run-cache-test", "", "", context.Background())
	t.Cleanup(r.cleanupScratch)
	return a, mock, r
}

// withSizeForCache temporarily lowers the spill threshold so tests don't need
// 10 MiB payloads.
func withSizeForCache(t *testing.T, n int) {
	t.Helper()
	old := sizeForCache
	sizeForCache = n
	t.Cleanup(func() { sizeForCache = old })
}

func TestOpenCachedSmallFileNotCached(t *testing.T) {
	withSizeForCache(t, 1024)
	_, mock, r := storageAgent(t)
	mock.put("data/small.txt", []byte("hello world"))

	for i := 0; i < 2; i++ {
		rc, err := r.openCached(context.Background(), "data/small.txt")
		if err != nil {
			t.Fatalf("openCached: %v", err)
		}
		got, _ := readAllClose(rc)
		if string(got) != "hello world" {
			t.Fatalf("got %q", got)
		}
	}
	if n := mock.gets("data/small.txt"); n != 2 {
		t.Fatalf("small file should re-fetch each read; got %d GETs, want 2", n)
	}
	if len(r.fileCache.entries) != 0 {
		t.Fatalf("small file must not be cached; entries=%d", len(r.fileCache.entries))
	}
}

func TestOpenCachedLargeFileSpillsAndServesLocally(t *testing.T) {
	withSizeForCache(t, 1024)
	_, mock, r := storageAgent(t)
	big := bytes.Repeat([]byte("A"), 4096) // > threshold
	mock.put("data/big.bin", big)

	for i := 0; i < 3; i++ {
		rc, err := r.openCached(context.Background(), "data/big.bin")
		if err != nil {
			t.Fatalf("openCached #%d: %v", i, err)
		}
		got, _ := readAllClose(rc)
		if !bytes.Equal(got, big) {
			t.Fatalf("read #%d mismatch: got %d bytes", i, len(got))
		}
	}
	if n := mock.gets("data/big.bin"); n != 1 {
		t.Fatalf("large file should be fetched once then served from disk; got %d GETs, want 1", n)
	}
	if len(r.fileCache.entries) != 1 {
		t.Fatalf("large file should be cached; entries=%d", len(r.fileCache.entries))
	}
}

func TestInvalidateCacheRefetches(t *testing.T) {
	withSizeForCache(t, 1024)
	_, mock, r := storageAgent(t)
	big := bytes.Repeat([]byte("B"), 4096)
	mock.put("data/x.bin", big)

	readAllClose(mustOpen(t, r, "data/x.bin")) // populate cache (GET #1)
	r.invalidateCache("data/x.bin")
	if len(r.fileCache.entries) != 0 {
		t.Fatalf("invalidate should drop the entry")
	}
	readAllClose(mustOpen(t, r, "data/x.bin")) // re-fetch (GET #2)
	if n := mock.gets("data/x.bin"); n != 2 {
		t.Fatalf("after invalidate the next read must re-fetch; got %d GETs, want 2", n)
	}
}

func TestCleanupRemovesScratch(t *testing.T) {
	withSizeForCache(t, 1024)
	_, mock, r := storageAgent(t)
	mock.put("data/y.bin", bytes.Repeat([]byte("C"), 4096))
	readAllClose(mustOpen(t, r, "data/y.bin"))

	dir := r.fileCache.dir
	if dir == "" {
		t.Fatal("expected a scratch dir after a spill")
	}
	r.cleanupScratch()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %s should be gone (stat err=%v)", dir, err)
	}
	if len(r.fileCache.entries) != 0 {
		t.Fatalf("cleanup should clear entries")
	}
}

func TestLRUEviction(t *testing.T) {
	withSizeForCache(t, 1024)
	old := fileCacheQuota
	fileCacheQuota = 10000
	t.Cleanup(func() { fileCacheQuota = old })
	_, mock, r := storageAgent(t)

	// Three ~4 KiB files; quota holds two. Reading a→b→a→c should evict b
	// (least recently used), keeping a and c.
	for _, name := range []string{"a", "b", "c"} {
		mock.put("d/"+name, bytes.Repeat([]byte(name), 4096))
	}
	readAllClose(mustOpen(t, r, "d/a"))
	readAllClose(mustOpen(t, r, "d/b"))
	readAllClose(mustOpen(t, r, "d/a")) // refresh a's LRU position (served from disk)
	readAllClose(mustOpen(t, r, "d/c")) // admits c → evicts b

	if _, ok := r.fileCache.entries["d/b"]; ok {
		t.Fatal("d/b should have been evicted as LRU")
	}
	if _, ok := r.fileCache.entries["d/a"]; !ok {
		t.Fatal("d/a should remain (recently used)")
	}
	if _, ok := r.fileCache.entries["d/c"]; !ok {
		t.Fatal("d/c should remain (just added)")
	}
}

func mustOpen(t *testing.T, r *run, path string) io.ReadCloser {
	t.Helper()
	got, err := r.openCached(context.Background(), path)
	if err != nil {
		t.Fatalf("openCached %s: %v", path, err)
	}
	return got
}
