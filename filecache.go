package agentsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
)

// sizeForCache is the threshold above which a streamed read spills to a
// local-disk cache file so later reads of the same path hit local disk instead
// of re-streaming from S3. Files at or below it are cheap to re-fetch, so
// they're handed back inline and never cached.
var sizeForCache = 10 << 20 // 10 MiB

// fileCacheQuota bounds total local-disk bytes the per-run cache holds. On
// overflow the least-recently-used entries are evicted (backing files deleted)
// until the incoming entry fits. A single file larger than the quota is still
// admitted — the quota is a soft bound, not a hard reject.
var fileCacheQuota int64 = 2 << 30 // 2 GiB

// fileCache is a per-run, path-keyed local-disk read cache. A read that
// exceeds sizeForCache is spilled in full to a scratch file; later reads of
// the same canonical path are served from disk rather than re-streamed from
// S3. The whole scratch dir is removed at run end (run.cleanupScratch).
type fileCache struct {
	mu      sync.Mutex
	dir     string // scratch dir; "" until the first spill creates it
	entries map[string]*cacheEntry
	total   int64 // sum of entry sizes currently on disk
	seq     int   // monotonic tick driving LRU ordering
}

type cacheEntry struct {
	localPath string
	size      int64
	usedAt    int // seq value at last access
}

func newFileCache() *fileCache {
	return &fileCache{entries: make(map[string]*cacheEntry)}
}

// open returns a reader over the cached copy of canon, or ok=false on a miss.
// A hit refreshes the entry's LRU position. If the backing file has vanished
// the stale entry is dropped and treated as a miss.
func (fc *fileCache) open(canon string) (io.ReadCloser, bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	e, ok := fc.entries[canon]
	if !ok {
		return nil, false
	}
	f, err := os.Open(e.localPath)
	if err != nil {
		delete(fc.entries, canon)
		fc.total -= e.size
		return nil, false
	}
	fc.seq++
	e.usedAt = fc.seq
	return f, true
}

// spill drains src to a fresh scratch file and registers it under canon,
// replacing any existing entry and evicting LRU entries to stay under quota.
func (fc *fileCache) spill(canon string, src io.Reader) error {
	fc.mu.Lock()
	if fc.dir == "" {
		dir, err := os.MkdirTemp("", "airlock-run-cache-")
		if err != nil {
			fc.mu.Unlock()
			return fmt.Errorf("agentsdk: file cache: mkdir scratch: %w", err)
		}
		fc.dir = dir
	}
	dir := fc.dir
	fc.mu.Unlock()

	tmp, err := os.CreateTemp(dir, "f-*")
	if err != nil {
		return fmt.Errorf("agentsdk: file cache: create temp: %w", err)
	}
	size, copyErr := io.Copy(tmp, src)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("agentsdk: file cache: spill %s: %w", canon, copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("agentsdk: file cache: close spill %s: %w", canon, closeErr)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if old, ok := fc.entries[canon]; ok {
		os.Remove(old.localPath)
		fc.total -= old.size
		delete(fc.entries, canon)
	}
	fc.evictForLocked(size)
	fc.seq++
	fc.entries[canon] = &cacheEntry{localPath: tmp.Name(), size: size, usedAt: fc.seq}
	fc.total += size
	return nil
}

// evictForLocked drops least-recently-used entries until an incoming entry of
// need bytes fits under the quota. Caller holds fc.mu. A single file larger
// than the whole quota is admitted as-is (quota is a soft bound).
func (fc *fileCache) evictForLocked(need int64) {
	if need >= fileCacheQuota {
		return
	}
	for fc.total+need > fileCacheQuota && len(fc.entries) > 0 {
		var lruKey string
		lruUsed := math.MaxInt
		for k, e := range fc.entries {
			if e.usedAt < lruUsed {
				lruUsed = e.usedAt
				lruKey = k
			}
		}
		e := fc.entries[lruKey]
		os.Remove(e.localPath)
		fc.total -= e.size
		delete(fc.entries, lruKey)
	}
}

// invalidate drops the entry for canon (deleting its backing file). Called
// when a path is written, deleted, or otherwise mutated so a later read
// re-fetches fresh content.
func (fc *fileCache) invalidate(canon string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if e, ok := fc.entries[canon]; ok {
		os.Remove(e.localPath)
		fc.total -= e.size
		delete(fc.entries, canon)
	}
}

// cleanup removes the entire scratch dir and resets the cache. Idempotent.
func (fc *fileCache) cleanup() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.dir != "" {
		os.RemoveAll(fc.dir)
		fc.dir = ""
	}
	fc.entries = make(map[string]*cacheEntry)
	fc.total = 0
}

// openCached returns a reader for path, transparently using and populating the
// per-run local-disk cache. Files at or below sizeForCache stream straight
// from S3 uncached; larger files are spilled to disk in full on first read and
// served locally thereafter. The caller closes the returned reader.
func (r *run) openCached(ctx context.Context, path string) (io.ReadCloser, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	if rc, ok := r.fileCache.open(canon); ok {
		return rc, nil
	}
	src, err := r.agent.openFileRaw(ctx, canon)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	peek, err := io.ReadAll(io.LimitReader(src, int64(sizeForCache)+1))
	if err != nil {
		return nil, err
	}
	if len(peek) <= sizeForCache {
		// Small file — return inline, don't bother caching.
		return io.NopCloser(bytes.NewReader(peek)), nil
	}
	// Large file — complete the spill, then serve from the local copy.
	if err := r.fileCache.spill(canon, io.MultiReader(bytes.NewReader(peek), src)); err != nil {
		return nil, err
	}
	rc, ok := r.fileCache.open(canon)
	if !ok {
		return nil, errors.New("agentsdk: file cache entry missing immediately after spill")
	}
	return rc, nil
}

// invalidateCache drops any cached copy of path. Best-effort: a path that
// won't normalize was never cached, so there's nothing to drop.
func (r *run) invalidateCache(path string) {
	canon, err := normalizePath(path)
	if err != nil {
		return
	}
	r.fileCache.invalidate(canon)
}

// cleanupScratch removes the run's local cache scratch dir. Guarded so the
// run's finalizer (run.complete) can call it on every terminal path.
func (r *run) cleanupScratch() {
	r.cleanupOnce.Do(func() {
		if r.fileCache != nil {
			r.fileCache.cleanup()
		}
	})
}
