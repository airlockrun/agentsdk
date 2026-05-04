package agentsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// reservedTmpPath is the framework-owned scratch directory used by run_js
// output truncation and media generation. Builders may RegisterDirectory
// at this path — the register helper preserves the framework caps but
// allows a custom Description.
const reservedTmpPath = "tmp"

// ErrNotFound is returned by CheckFileAccess and the storage methods for
// both "directory not registered" and "caller does not have access" — the
// two cases are deliberately indistinguishable at the public surface so
// path-guessing leaks no information about what exists.
var ErrNotFound = errors.New("agentsdk: file not found")

// ErrInvalidPath is returned for paths that fail normalization (missing
// leading '/', empty segments, '..' segments, etc.).
var ErrInvalidPath = errors.New("agentsdk: invalid path")

// --- Caller plumbing ---

// Caller carries the access level of whoever triggered the current
// dispatch. Framework dispatch sites (tool Execute, VM bindings, cron,
// webhook, route, subdomain proxy) inject one onto ctx via WithCaller.
// Builder Go code that constructs paths itself does NOT need to set a
// caller — it calls the trusted file API directly (OpenFile/ReadFile/
// WriteFile/StatFile/ListDir/DeleteFile) which bypasses CheckFileAccess.
type Caller struct {
	Access Access
	UserID string // optional, for audit
	RunID  string // optional, for audit
}

type callerCtxKey struct{}

// WithCaller attaches a Caller to ctx. Used by the framework when
// dispatching into untrusted territory (LLM-driven VM, public HTTP).
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// CallerFrom returns the Caller attached to ctx, defaulting to
// AccessPublic when none is set. This is the fail-closed default:
// forgetting to tag ctx denies access to anything user-or-above.
func CallerFrom(ctx context.Context) Caller {
	if v, ok := ctx.Value(callerCtxKey{}).(Caller); ok {
		if v.Access == "" {
			v.Access = AccessPublic
		}
		return v
	}
	return Caller{Access: AccessPublic}
}

// --- Path normalization ---

// normalizePath enforces the storage-path conventions:
//   - no leading '/' (paths are S3-style: "uploads/x.csv", not
//     "/uploads/x.csv"). Leading slash is a hard error so the LLM and
//     builders converge on one form.
//   - no trailing '/' (canonical form has none)
//   - no empty segment ('//')
//   - no '.' or '..' segment
//   - non-empty
//
// Returns the canonical path or ErrInvalidPath.
func normalizePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if p[0] == '/' {
		return "", fmt.Errorf("%w: must be slashless (S3-style); got %q with leading '/'", ErrInvalidPath, p)
	}
	// Strip trailing slash.
	if p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if p == "" {
		return "", fmt.Errorf("%w: path is empty after trimming '/'", ErrInvalidPath)
	}
	// Walk segments, reject empty (//), '.', '..'.
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return "", fmt.Errorf("%w: empty segment ('//' in path)", ErrInvalidPath)
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: '%s' segments are not allowed", ErrInvalidPath, seg)
		}
	}
	return p, nil
}

// pathHasPrefix reports whether `p` lies under directory `dir`. Both must
// be canonical (slashless, no trailing slash). A directory is its own
// prefix only at directory granularity: dir="reports" matches p="reports/x"
// but NOT p="reportsx" — the segment boundary matters.
func pathHasPrefix(p, dir string) bool {
	if p == dir {
		return true
	}
	if !strings.HasPrefix(p, dir) {
		return false
	}
	return p[len(dir)] == '/'
}

// --- Directory lookup ---

// lookupDirectory finds the registered directory whose path is the
// longest prefix of `p` (post-normalization). Returns nil if no
// directory covers `p`. Caller must have already normalized `p`.
func (a *Agent) lookupDirectory(p string) *Directory {
	var best *Directory
	for _, d := range a.directories {
		if !pathHasPrefix(p, d.Path) {
			continue
		}
		if best == nil || len(d.Path) > len(best.Path) {
			best = d
		}
	}
	return best
}

// dirCap returns the directory's access cap for `op`. Delete folds into
// Write. Unknown ops fall back to AccessAdmin (deny all but admin) so
// future op tags fail closed if added without updating this switch.
func dirCap(d *Directory, op FileOp) Access {
	switch op {
	case OpRead:
		return d.Read
	case OpWrite:
		return d.Write
	case OpList:
		return d.List
	}
	return AccessAdmin
}

// hasPublicDirCap reports whether at least one registered directory grants
// AccessPublic for the given op. Used at VM bind time so file primitives
// (readFile, writeFile, listDir, etc.) appear in a public-caller's
// runtime only when there's actually some directory they could touch —
// keeps the public attack surface tight and avoids dangling bindings
// that would just throw on every CheckFileAccess.
func (a *Agent) hasPublicDirCap(op FileOp) bool {
	for _, d := range a.directories {
		if dirCap(d, op) == AccessPublic {
			return true
		}
	}
	return false
}

// --- Public access gate ---

// CheckFileAccess is the single gate for paths that arrived from
// untrusted territory: VM run_js code, HTTP requests, tool inputs from
// the LLM. Builder Go code that constructs paths itself bypasses this
// check by calling OpenFile/ReadFile/WriteFile/etc. directly.
//
// Returns ErrInvalidPath for malformed paths, ErrNotFound for everything
// else (denied OR no covering directory). The two latter cases are
// indistinguishable on purpose so path-guessing reveals nothing.
func (a *Agent) CheckFileAccess(ctx context.Context, path string, op FileOp) error {
	canon, err := normalizePath(path)
	if err != nil {
		return err
	}
	d := a.lookupDirectory(canon)
	if d == nil {
		return ErrNotFound
	}
	cap := dirCap(d, op)
	caller := CallerFrom(ctx)
	if !accessSatisfies(caller.Access, cap) {
		return ErrNotFound
	}
	return nil
}

// --- Trusted Go file API ---

// OpenFile streams a file. The returned ReadCloser must be closed by the
// caller. Trusted: no access check. Used by builder Go code that
// constructs paths itself.
func (a *Agent) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	return a.openFileRaw(ctx, canon)
}

// ReadFile reads a file fully into memory. For very large files prefer
// OpenFile + io.Copy. Trusted: no access check.
func (a *Agent) ReadFile(ctx context.Context, path string) ([]byte, error) {
	rc, err := a.OpenFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// WriteFile writes data with the given content type. Returns the resulting
// FileInfo (path/filename/contentType/size/lastModified). Trusted: no
// access check.
func (a *Agent) WriteFile(ctx context.Context, path string, data io.Reader, contentType string) (FileInfo, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return FileInfo{}, err
	}
	// Buffer to learn the size; the API path needs Content-Length.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, data)
	if err != nil {
		return FileInfo{}, fmt.Errorf("agentsdk: WriteFile %s: read input: %w", canon, err)
	}
	if err := a.writeFileRaw(ctx, canon, &buf, contentType, ""); err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		Path:         canon,
		Filename:     pathBase(canon),
		ContentType:  contentType,
		Size:         n,
		LastModified: time.Now(),
	}, nil
}

// StatFile returns metadata for a file. Trusted: no access check.
func (a *Agent) StatFile(ctx context.Context, path string) (FileInfo, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return FileInfo{}, err
	}
	return a.statFileRaw(ctx, canon)
}

// ListOpts controls ListDir.
type ListOpts struct {
	// Recursive walks the entire subtree. Zero value (false) lists only
	// files directly under the path (one level only, like `ls`).
	Recursive bool
}

// ListDir enumerates files under `path`. Trusted: no access check. The
// empty string lists the agent root.
func (a *Agent) ListDir(ctx context.Context, path string, opts ListOpts) ([]FileInfo, error) {
	// path is a directory prefix; trailing slash is allowed (and expected
	// for clarity), normalizePath rejects it for files.
	prefix := strings.TrimRight(path, "/")
	if prefix != "" {
		if _, err := normalizePath(prefix); err != nil {
			return nil, err
		}
	}
	return a.listDirRaw(ctx, prefix, opts.Recursive)
}

// DeleteFile removes a file. Idempotent — missing files do not error.
// Trusted: no access check.
func (a *Agent) DeleteFile(ctx context.Context, path string) error {
	canon, err := normalizePath(path)
	if err != nil {
		return err
	}
	return a.deleteFileRaw(ctx, canon)
}

// CopyFile server-side-copies a file from src to dst. Both paths are
// absolute and may live under different directories. Trusted: no access
// check.
func (a *Agent) CopyFile(ctx context.Context, src, dst string) error {
	srcCanon, err := normalizePath(src)
	if err != nil {
		return err
	}
	dstCanon, err := normalizePath(dst)
	if err != nil {
		return err
	}
	return a.copyFileRaw(ctx, srcCanon, dstCanon)
}

// ShareFileURL returns a presigned, unauthenticated, time-limited URL
// pointing at the given storage path. ttl <= 0 picks the server default
// (1h); the server caps anything over 24h. The URL is signed for the
// public S3 endpoint when configured, so it works from outside the docker
// network (browsers, LLM providers, external tools). Trusted: no access
// check — the JS binding gates LLM-supplied paths via CheckFileAccess.
//
// Use cases: embedding in markdown ([file](url)), sharing externally,
// cases where the agent's authenticated /__air/storage subdomain route
// isn't reachable for the recipient. For showing files in chat, prefer
// printToUser({type:"file", source:path}).
func (a *Agent) ShareFileURL(ctx context.Context, path string, ttl time.Duration) (*ShareFileResponse, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	body := ShareFileRequest{
		Path:           canon,
		ExpiresSeconds: int64(ttl.Seconds()),
	}
	var resp ShareFileResponse
	if err := a.client.doJSON(ctx, "POST", "/api/agent/storage/share", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- Internal helpers ---

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// publicURLForPath returns the URL at which `path` is fetchable on the
// agent's subdomain, e.g. "https://slug.example.com/__air/storage/reports/q1.csv".
// Whether the URL succeeds depends on the directory's Read cap and the
// caller's auth state — see serveStoragePath on the airlock side.
func (a *Agent) publicURLForPath(path string) string {
	return a.publicStorageBaseSnapshot() + "/" + path
}

// --- HTTP client (raw helpers — Trusted Go API wraps these) ---

func (a *Agent) writeFileRaw(ctx context.Context, path string, data io.Reader, contentType, originalFilename string) error {
	req, err := a.client.newRequest(ctx, "PUT", "/api/agent/storage/"+path, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if originalFilename != "" {
		req.Header.Set("X-Filename", originalFilename)
	}
	resp, err := a.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentsdk: writeFile %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: writeFile %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) openFileRaw(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := a.client.do(ctx, "GET", "/api/agent/storage/"+path, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("agentsdk: openFile %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, nil
}

func (a *Agent) deleteFileRaw(ctx context.Context, path string) error {
	resp, err := a.client.do(ctx, "DELETE", "/api/agent/storage/"+path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: deleteFile %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) statFileRaw(ctx context.Context, path string) (FileInfo, error) {
	body := struct {
		Path string `json:"path"`
	}{path}
	var info FileInfo
	if err := a.client.doJSON(ctx, "POST", "/api/agent/storage/info", body, &info); err != nil {
		return FileInfo{}, err
	}
	if info.Path == "" {
		info.Path = path
	}
	return info, nil
}

func (a *Agent) copyFileRaw(ctx context.Context, src, dst string) error {
	body := struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}{src, dst}
	return a.client.doJSON(ctx, "POST", "/api/agent/storage/copy", body, nil)
}

func (a *Agent) listDirRaw(ctx context.Context, path string, recursive bool) ([]FileInfo, error) {
	q := url.Values{}
	q.Set("path", path)
	if recursive {
		q.Set("recursive", "true")
	}
	var files []FileInfo
	if err := a.client.doJSON(ctx, "GET", "/api/agent/storage?"+q.Encode(), nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}
