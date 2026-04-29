package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// StorageRef is a typed reference to a file in a registered Storage zone.
// The fields are unexported so refs can only come from a *StorageHandle —
// either via handle.Ref(key), or by JSON-unmarshaling a wire-format
// {"zone": "...", "key": "..."} that the framework hands back (e.g.
// httpRequest savedTo, generateImage result, builder tool returns).
//
// Use it as the field type on builder Out structs so the LLM sees an
// unambiguous {zone, key} shape:
//
//	type FetchOut struct {
//	    File agentsdk.StorageRef `json:"file"`
//	}
//
// The corresponding JS binding is `storage_<zone>` — JS code reads the
// referenced file with storage_X.get(ref) (the binding validates the ref's
// zone matches its own slug).
type StorageRef struct {
	zone string
	key  string
}

// Zone returns the zone slug.
func (r StorageRef) Zone() string { return r.zone }

// Key returns the file key relative to the zone.
func (r StorageRef) Key() string { return r.key }

// String returns the composed "zone/key" form, useful for logging.
func (r StorageRef) String() string {
	if r.zone == "" {
		return ""
	}
	return r.zone + "/" + r.key
}

// MarshalJSON encodes the ref as {"zone":"...","key":"..."}.
func (r StorageRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(storageRefWire{Zone: r.zone, Key: r.key})
}

// UnmarshalJSON decodes {"zone":"...","key":"..."}. The framework trusts
// the input here — validation happens at use time (when a JS binding
// receives a ref, or when builder code looks the zone up).
func (r *StorageRef) UnmarshalJSON(data []byte) error {
	var w storageRefWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	r.zone = w.Zone
	r.key = w.Key
	return nil
}

type storageRefWire struct {
	Zone string `json:"zone"`
	Key  string `json:"key"`
}

// reservedTmpSlug is the framework-owned scratch zone used by run_js
// output truncation and media generation. Builders may register a
// Storage with this slug — RegisterStorage silently accepts it but
// keeps the framework's Access/Description; both sides share the same
// handle.
const reservedTmpSlug = "tmp"

// StorageHandle is a compile-time binding to a registered Storage zone.
// All keys passed to its methods are relative to the zone's prefix; the
// handle prepends "{slug}/" before talking to Airlock. List returns keys
// stripped of the prefix as well, so callers see relative paths.
type StorageHandle struct {
	slug  string
	read  Access // who may invoke Get/Stat/List from JS (and the public route)
	write Access // who may invoke Put/Delete/Copy from JS
	agent *Agent
}

// Slug returns the zone's slug. Useful when constructing public URLs
// (storage.airlock.example.com/storage/{agentID}/{slug}/{key}).
func (h *StorageHandle) Slug() string { return h.slug }

// Ref returns a typed StorageRef for `key` in this zone. This is the only
// public way to construct a StorageRef — builder code that returns file
// references from tool Out structs goes through here, so a ref can never
// claim a zone that the handle doesn't represent.
func (h *StorageHandle) Ref(key string) StorageRef {
	return StorageRef{zone: h.slug, key: key}
}

// ReadAccess returns the zone's required level for reads.
func (h *StorageHandle) ReadAccess() Access { return h.read }

// WriteAccess returns the zone's required level for writes.
func (h *StorageHandle) WriteAccess() Access { return h.write }

func (h *StorageHandle) zoneKey(rel string) string {
	rel = strings.TrimLeft(rel, "/")
	return h.slug + "/" + rel
}

// Put writes a file at `key` (relative to this zone) with the given
// Content-Type. data is fully read; large bodies are streamed.
func (h *StorageHandle) Put(ctx context.Context, key string, data io.Reader, contentType string) error {
	return h.agent.storagePut(ctx, h.zoneKey(key), data, contentType)
}

// Get returns a reader over the file at `key` (relative to this zone).
// The caller must Close the returned ReadCloser.
func (h *StorageHandle) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return h.agent.storageGet(ctx, h.zoneKey(key))
}

// Delete removes the file at `key` (relative to this zone). Idempotent —
// missing files do not error.
func (h *StorageHandle) Delete(ctx context.Context, key string) error {
	return h.agent.storageDelete(ctx, h.zoneKey(key))
}

// Stat returns metadata for `key` (relative to this zone). The returned
// StoredFile.Key is also relative.
func (h *StorageHandle) Stat(ctx context.Context, key string) (StoredFile, error) {
	info, err := h.agent.storageStat(ctx, h.zoneKey(key))
	if err != nil {
		return StoredFile{}, err
	}
	info.Key = stripPrefix(info.Key, h.slug+"/")
	return info, nil
}

// List enumerates files under `prefix` (relative to this zone). Returned
// StoredFile.Key values are also relative.
func (h *StorageHandle) List(ctx context.Context, prefix string) ([]StoredFile, error) {
	files, err := h.agent.storageList(ctx, h.zoneKey(prefix))
	if err != nil {
		return nil, err
	}
	for i := range files {
		files[i].Key = stripPrefix(files[i].Key, h.slug+"/")
	}
	return files, nil
}

// Copy server-side-copies a file within this zone. Both keys are relative.
func (h *StorageHandle) Copy(ctx context.Context, src, dst string) error {
	return h.agent.storageCopy(ctx, h.zoneKey(src), h.zoneKey(dst))
}

// CopyTo server-side-copies a file from this zone into another zone.
// dstKey is relative to dstZone; src is relative to this zone. Builders
// compose move = CopyTo + src.Delete.
func (h *StorageHandle) CopyTo(ctx context.Context, src string, dstZone *StorageHandle, dst string) error {
	if dstZone == nil {
		return fmt.Errorf("agentsdk: StorageHandle.CopyTo: dstZone is nil")
	}
	return h.agent.storageCopy(ctx, h.zoneKey(src), dstZone.zoneKey(dst))
}

// URL returns the URL at which the given key is fetchable on the agent's
// subdomain. Whether a request to that URL succeeds depends on the zone's
// Read level and the caller's auth state:
//
//   - AccessPublic:  served unauthenticated.
//   - AccessUser:    requires a valid agent-subdomain session cookie + agent
//                    membership; the proxy redirects through the login flow
//                    when the cookie is absent (so a click in chat triggers
//                    sign-in and lands back on the file).
//   - AccessAdmin:   same, but requires admin role on the agent.
//   - AccessInternal: the proxy 404s the URL — internal zones are builder-Go
//                     only. URL still composes a string so callers don't get
//                     silent empty hrefs, but a warning is logged so the
//                     mistake is visible.
//
// Re-resolves on the next sync if the agent's slug or the configured
// domain changes.
func (h *StorageHandle) URL(key string) string {
	if h.read == AccessInternal {
		h.agent.logger.Warn("StorageHandle.URL called on AccessInternal zone — the URL will 404",
			zap.String("zone", h.slug), zap.String("key", key))
	}
	return h.agent.publicStorageBaseSnapshot() + "/" + h.slug + "/" + strings.TrimLeft(key, "/")
}

func stripPrefix(s, p string) string {
	if strings.HasPrefix(s, p) {
		return s[len(p):]
	}
	return s
}

// --- Agent-internal storage helpers (only StorageHandle / framework call these) ---

func (a *Agent) storagePut(ctx context.Context, key string, data io.Reader, contentType string) error {
	req, err := a.client.newRequest(ctx, "PUT", "/api/agent/storage/"+key, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := a.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentsdk: storage put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: storage put %s: status %d: %s", key, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) storageGet(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := a.client.do(ctx, "GET", "/api/agent/storage/"+key, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("agentsdk: storage get %s: status %d", key, resp.StatusCode)
	}
	return resp.Body, nil
}

func (a *Agent) storageDelete(ctx context.Context, key string) error {
	resp, err := a.client.do(ctx, "DELETE", "/api/agent/storage/"+key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: storage delete %s: status %d: %s", key, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) storageStat(ctx context.Context, key string) (StoredFile, error) {
	body := struct {
		Key string `json:"key"`
	}{key}
	var info StoredFile
	if err := a.client.doJSON(ctx, "POST", "/api/agent/storage/info", body, &info); err != nil {
		return StoredFile{}, err
	}
	return info, nil
}

func (a *Agent) storageCopy(ctx context.Context, src, dst string) error {
	body := struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}{src, dst}
	return a.client.doJSON(ctx, "POST", "/api/agent/storage/copy", body, nil)
}

func (a *Agent) storageList(ctx context.Context, prefix string) ([]StoredFile, error) {
	path := "/api/agent/storage"
	if prefix != "" {
		path += "?prefix=" + url.QueryEscape(prefix)
	}
	var files []StoredFile
	if err := a.client.doJSON(ctx, "GET", path, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// findStorageByKey returns the registered zone whose prefix matches the
// given full key (e.g. "uploads/doc.pdf" → the uploads zone). Used by
// vm.go's attachToContext to validate a JS-supplied "{slug}/{key}" string.
func (a *Agent) findStorageByKey(key string) (*StorageHandle, string, bool) {
	idx := strings.IndexByte(key, '/')
	if idx <= 0 {
		return nil, "", false
	}
	slug := key[:idx]
	rel := key[idx+1:]
	zone, ok := a.storages[slug]
	if !ok {
		return nil, "", false
	}
	return &StorageHandle{slug: slug, read: zone.Read, write: zone.Write, agent: a}, rel, true
}
