package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

// reservedTmpPath is the framework-owned scratch directory used by run_js
// output truncation and media generation. Builders may RegisterDirectory
// at this path — the register helper preserves the framework caps but
// allows a custom Description.
const reservedTmpPath = "tmp"

// reservedIncomingPath is the framework-owned ephemeral directory where
// airlock writes files sent to this agent as A2A tool arguments or as
// inline uploads from external MCP clients. Tool bodies don't reference
// it directly — args are rewritten at the boundary, so the body
// receives a path inside this prefix and readFiles it like any other
// path. Sub-paths carry a scope key (`run-{uuid}` or `conv-{uuid}`);
// ResolveFilePath gates reads on that scope matching the current run's
// caller context, so callers cannot read other callers' uploads even
// when both are anonymous. Files are auto-cleaned by retention.
const reservedIncomingPath = "__incoming"

// reservedSiblingsPath is the framework-owned directory where airlock
// writes files returned from sibling agents' tools. Caller's run_js
// code receives paths like "siblings/imagebot/results/cropped.png" in
// tool results and can keep working with them as if they were locally
// produced. Files are auto-cleaned by retention.
const reservedSiblingsPath = "siblings"

// ErrNotFound is returned by ResolveFilePath and the storage methods for
// both "directory not registered" and "caller does not have access" — the
// two cases are deliberately indistinguishable at the public surface so
// path-guessing leaks no information about what exists.
var ErrNotFound = errors.New("agentsdk: file not found")

// ErrInvalidPath is returned for paths that fail normalization (missing
// leading '/', empty segments, '..' segments, etc.).
var ErrInvalidPath = errors.New("agentsdk: invalid path")

// --- Caller plumbing ---

// caller carries the access level of whoever triggered the current
// dispatch. Framework dispatch sites (tool Execute, VM bindings, cron,
// webhook, route, subdomain proxy) inject one onto ctx via withCaller.
// Builder Go code that constructs paths itself does NOT need to set a
// caller — it calls the trusted file API directly (OpenFile/ReadFile/
// WriteFile/StatFile/ListDir/DeleteFile) which bypasses ResolveFilePath.
type caller struct {
	Access Access
	UserID string // optional, for audit
	RunID  string // optional, for audit
}

type callerCtxKey struct{}

// withCaller attaches a caller to ctx. Used by the framework when
// dispatching into untrusted territory (LLM-driven VM, public HTTP).
func withCaller(ctx context.Context, c caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// callerFromContext returns the caller attached to ctx, defaulting to
// AccessPublic when none is set. This is the fail-closed default:
// forgetting to tag ctx denies access to anything user-or-above.
func callerFromContext(ctx context.Context) caller {
	if v, ok := ctx.Value(callerCtxKey{}).(caller); ok {
		if v.Access == "" {
			v.Access = AccessPublic
		}
		return v
	}
	if r := runFromContext(ctx); r != nil {
		access := r.callerAccess
		if access == "" {
			access = AccessPublic
		}
		return caller{Access: access, UserID: r.userID, RunID: r.id}
	}
	if l := lazyRunFromContext(ctx); l != nil {
		access := l.callerAccess
		if access == "" {
			access = AccessPublic
		}
		return caller{Access: access, UserID: l.userID, RunID: l.parentRunID}
	}
	if test, ok := testcaller.FromContext(ctx); ok {
		return caller{Access: Access(test.Access), UserID: test.UserID}
	}
	return caller{Access: AccessPublic}
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
	if !utf8.ValidString(p) || strings.ContainsRune(p, '\x00') || strings.ContainsRune(p, '\\') {
		return "", fmt.Errorf("%w: path contains invalid characters", ErrInvalidPath)
	}
	// Walk segments, reject empty (//), '.', '..', and control characters.
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return "", fmt.Errorf("%w: empty segment ('//' in path)", ErrInvalidPath)
		}
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: '%s' segments are not allowed", ErrInvalidPath, seg)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("%w: path contains control characters", ErrInvalidPath)
			}
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
func (a *Agent) lookupDirectory(p string) *directory {
	var best *directory
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

func dirCap(d *directory, op FileOperation) (Access, bool) {
	switch op {
	case FileOperationRead:
		return d.Read, true
	case FileOperationList:
		return d.List, true
	case FileOperationWrite, FileOperationOverwrite, FileOperationDelete:
		return d.Write, true
	}
	return "", false
}

// hasPublicDirCap reports whether at least one registered directory grants
// AccessPublic for the given op. Used at VM bind time so file primitives
// (fileRead, fileWrite, fileList, etc.) appear in a public-caller's
// runtime only when there's actually some directory they could touch —
// keeps the public attack surface tight and avoids dangling bindings
// that would just throw on every ResolveFilePath.
func (a *Agent) hasPublicDirCap(op FileOperation) bool {
	for _, d := range a.directories {
		if cap, ok := dirCap(d, op); ok && cap == AccessPublic {
			return true
		}
	}
	return false
}

// --- Public access gate ---

// ResolveFilePath authorizes an untrusted path and returns the exact physical
// path that storage operations must use. Trusted Go storage methods bypass it.
func (a *Agent) ResolveFilePath(ctx context.Context, path string, op FileOperation) (FilePath, error) {
	if _, ok := dirCap(&directory{}, op); !ok {
		return "", fmt.Errorf("agentsdk: unsupported file operation %q", op)
	}
	canon, err := normalizePath(path)
	if err != nil {
		return "", err
	}
	d := a.lookupDirectory(canon)
	if d == nil {
		return "", ErrNotFound
	}
	caller := callerFromContext(ctx)
	if caller.Access == AccessAdmin {
		return FilePath(canon), nil
	}
	if d.incomingProvenance {
		return resolveIncomingPath(ctx, d, canon, op)
	}
	cap, _ := dirCap(d, op)
	if !accessSatisfies(caller.Access, cap) {
		return "", ErrNotFound
	}
	if d.Scope == ScopeNone {
		return FilePath(canon), nil
	}
	identity := fileIdentityFromContext(ctx)
	expected := identity.scopeSegment(d.Scope)
	if expected == "" {
		return "", ErrNotFound
	}
	if canon == d.Path {
		if op == FileOperationList {
			return FilePath(d.Path + "/" + expected), nil
		}
		if op == FileOperationWrite {
			return "", fmt.Errorf("%w: write path must include a filename", ErrInvalidPath)
		}
		return "", ErrNotFound
	}
	rest := canon[len(d.Path)+1:]
	segment, _, _ := strings.Cut(rest, "/")
	if segment == expected {
		return FilePath(canon), nil
	}
	if isScopeSegment(segment) {
		return "", ErrNotFound
	}
	if op == FileOperationWrite || op == FileOperationList {
		return FilePath(d.Path + "/" + expected + "/" + rest), nil
	}
	return "", ErrNotFound
}

func resolveIncomingPath(ctx context.Context, d *directory, canon string, op FileOperation) (FilePath, error) {
	if op != FileOperationRead || canon == d.Path {
		return "", ErrNotFound
	}
	rest := canon[len(d.Path)+1:]
	segment, _, _ := strings.Cut(rest, "/")
	identity := fileIdentityFromContext(ctx)
	allowed := []string{}
	if identity.userID != "" {
		allowed = append(allowed, "user-"+identity.userID)
	}
	if identity.conversationID != "" {
		allowed = append(allowed, "conv-"+identity.conversationID)
	}
	if identity.parentRunID != "" {
		allowed = append(allowed, "run-"+identity.parentRunID)
	}
	for _, expected := range allowed {
		if segment == expected {
			return FilePath(canon), nil
		}
	}
	return "", ErrNotFound
}

func (a *Agent) resolveFilePath(ctx context.Context, path string, op FileOperation) (string, error) {
	resolved, err := a.ResolveFilePath(ctx, path, op)
	return string(resolved), err
}

type fileIdentity struct {
	userID         string
	conversationID string
	runID          string
	parentRunID    string
}

func fileIdentityFromContext(ctx context.Context) fileIdentity {
	if r := runFromContext(ctx); r != nil {
		return fileIdentity{userID: r.userID, conversationID: r.conversationID, runID: r.id, parentRunID: r.parentRunID}
	}
	if l := lazyRunFromContext(ctx); l != nil {
		identity := fileIdentity{userID: l.userID, conversationID: l.conversationID, parentRunID: l.parentRunID}
		if materialized := l.materialized(); materialized != nil {
			identity.runID = materialized.id
		}
		return identity
	}
	if test, ok := testcaller.FromContext(ctx); ok {
		return fileIdentity{userID: test.UserID}
	}
	return fileIdentity{}
}

func (i fileIdentity) scopeSegment(scope DirectoryScope) string {
	switch scope {
	case ScopeUser:
		if i.userID != "" {
			return "user-" + i.userID
		}
	case ScopeConversation:
		if i.conversationID != "" {
			return "conv-" + i.conversationID
		}
	case ScopeRun:
		if i.runID != "" {
			return "run-" + i.runID
		}
	}
	return ""
}

func isScopeSegment(segment string) bool {
	return strings.HasPrefix(segment, "user-") || strings.HasPrefix(segment, "conv-") || strings.HasPrefix(segment, "run-")
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

// OpenFileRange streams the inclusive byte range [start, end] of a file
// (HTTP Range semantics). The returned ReadCloser must be closed by the
// caller. Trusted: no access check.
func (a *Agent) OpenFileRange(ctx context.Context, path string, start, end int64) (io.ReadCloser, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	return a.openFileRangeRaw(ctx, canon, start, end)
}

// ReadRange reads the inclusive byte range [start, end] of a file fully into
// memory. Trusted: no access check.
func (a *Agent) ReadRange(ctx context.Context, path string, start, end int64) ([]byte, error) {
	if gw := goWallFrom(ctx); gw != nil {
		gw.enter()
		defer gw.exit()
	}
	rc, err := a.OpenFileRange(ctx, path, start, end)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ReadFile reads a file fully into memory. For very large files prefer
// OpenFile + io.Copy. Trusted: no access check.
func (a *Agent) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// The body read (io.ReadAll) dominates for large files and happens after
	// client.do returns headers, so credit the whole op to the go-call
	// accumulator (nesting-safe with the inner client.do span).
	if gw := goWallFrom(ctx); gw != nil {
		gw.enter()
		defer gw.exit()
	}
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
		Path:         FilePath(canon),
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
	noUnkeyedLiterals

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
// check — the JS binding resolves LLM-supplied paths via ResolveFilePath.
//
// Use cases: embedding in markdown ([file](url)), sharing externally,
// cases where the agent's authenticated /__air/storage subdomain route
// isn't reachable for the recipient. For showing files in chat, prefer
// output({type:"file", source:path}).
func (a *Agent) ShareFileURL(ctx context.Context, path string, ttl time.Duration) (*ShareFileResponse, error) {
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	body := wire.ShareFileRequest{
		Path:           canon,
		ExpiresSeconds: int64(ttl.Seconds()),
	}
	var resp wire.ShareFileResponse
	if err := a.client.doJSON(ctx, "POST", "/api/agent/storage/share", body, &resp); err != nil {
		return nil, err
	}
	return &ShareFileResponse{URL: resp.URL, ExpiresAtMs: resp.ExpiresAtMs}, nil
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
	return a.publicStorageBaseSnapshot() + "/" + escapeStoragePath(path)
}

// --- HTTP client (raw helpers — Trusted Go API wraps these) ---

func (a *Agent) writeFileRaw(ctx context.Context, path string, data io.Reader, contentType, originalFilename string) error {
	req, err := a.client.newRequest(ctx, "PUT", "/api/agent/storage/"+escapeStoragePath(path), data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if originalFilename != "" {
		req.Header.Set("X-Filename", originalFilename)
	}
	resp, err := a.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentsdk: fileWrite %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: fileWrite %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) openFileRaw(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := a.client.do(ctx, "GET", "/api/agent/storage/"+escapeStoragePath(path), nil)
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

// openFileRangeRaw streams the inclusive byte range [start, end] via a ranged
// GET. A satisfiable range returns 206; the caller closes resp.Body.
func (a *Agent) openFileRangeRaw(ctx context.Context, path string, start, end int64) (io.ReadCloser, error) {
	resp, err := a.client.getRange(ctx, "/api/agent/storage/"+escapeStoragePath(path), start, end)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("agentsdk: openFileRange %s: status %d", path, resp.StatusCode)
	}
	return resp.Body, nil
}

func (a *Agent) deleteFileRaw(ctx context.Context, path string) error {
	resp, err := a.client.do(ctx, "DELETE", "/api/agent/storage/"+escapeStoragePath(path), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentsdk: fileDelete %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (a *Agent) statFileRaw(ctx context.Context, path string) (FileInfo, error) {
	body := struct {
		Path string `json:"path"`
	}{path}
	data, err := json.Marshal(body)
	if err != nil {
		return FileInfo{}, err
	}
	resp, err := a.client.do(ctx, "POST", "/api/agent/storage/info", bytes.NewReader(data))
	if err != nil {
		return FileInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return FileInfo{}, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(resp.Body)
		return FileInfo{}, fmt.Errorf("agentsdk: statFile %s: status %d: %s", path, resp.StatusCode, string(responseBody))
	}
	var info wire.FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return FileInfo{}, fmt.Errorf("agentsdk: statFile %s: decode response: %w", path, err)
	}
	if info.Path == "" {
		info.Path = path
	}
	return fileInfoFromWire(info), nil
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
	var files []wire.FileInfo
	if err := a.client.doJSON(ctx, "GET", "/api/agent/storage?"+q.Encode(), nil, &files); err != nil {
		return nil, err
	}
	out := make([]FileInfo, len(files))
	for i, info := range files {
		out[i] = fileInfoFromWire(info)
	}
	return out, nil
}

func escapeStoragePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
