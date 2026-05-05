package agentsdk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SyncDown copies every file under `prefix` from S3-backed storage into
// `localDir`. After the call, files that exist in S3 are present locally
// at the matching subpath (S3 → local mirror within the prefix). Files
// that exist *only* locally are left in place — this is sync, not a
// destructive mirror, so the runtime image's seeded /var/agent/bin
// survives the very first call when S3 is still empty.
//
// Sync semantics: a file is overwritten locally iff the remote object's
// LastModified is newer than the local file's mtime, or the local file
// is missing / a different size. After overwriting, the local mtime is
// set to the remote's LastModified so subsequent SyncUp/SyncDown rounds
// don't churn the same files back and forth.
//
// Files synced down are chmodded to 0755. The use case is binaries +
// data caches — both fine with the executable bit set. Set the mode
// explicitly after the call if you need finer control.
//
// Trusted: no access check (builder code that constructs paths itself).
//
// Use case: pair with SyncUp in a cron handler to persist self-updating
// binaries (e.g. `bun upgrade`, `freshclam`) across container restarts —
// the running container's local copy is the working copy, S3 is the
// durable record. See the agent-builder prompt for a full worked example.
func (a *Agent) SyncDown(ctx context.Context, prefix, localDir string) error {
	prefix = strings.TrimRight(prefix, "/")
	if prefix != "" {
		if _, err := normalizePath(prefix); err != nil {
			return fmt.Errorf("agentsdk: SyncDown: prefix: %w", err)
		}
	}
	if localDir == "" {
		return fmt.Errorf("agentsdk: SyncDown: localDir is required")
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("agentsdk: SyncDown: mkdir %s: %w", localDir, err)
	}

	listPath := prefix + "/"
	if prefix == "" {
		listPath = ""
	}
	files, err := a.ListDir(ctx, listPath, ListOpts{Recursive: true})
	if err != nil {
		return fmt.Errorf("agentsdk: SyncDown: list %s: %w", prefix, err)
	}

	stripPrefix := prefix + "/"
	for _, fi := range files {
		rel := fi.Path
		if prefix != "" {
			rel = strings.TrimPrefix(fi.Path, stripPrefix)
			if rel == "" || rel == fi.Path {
				continue
			}
		}
		localPath := filepath.Join(localDir, rel)

		// Skip if local already matches remote (size + mtime).
		if st, err := os.Stat(localPath); err == nil {
			if st.Size() == fi.Size && !fi.LastModified.After(st.ModTime()) {
				continue
			}
		}

		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return fmt.Errorf("agentsdk: SyncDown: mkdir %s: %w", filepath.Dir(localPath), err)
		}

		rc, err := a.OpenFile(ctx, fi.Path)
		if err != nil {
			return fmt.Errorf("agentsdk: SyncDown: open %s: %w", fi.Path, err)
		}
		// Atomic-ish write: copy to a sibling temp, then rename. Avoids
		// half-written binaries being execve'd by something that happened
		// to fire mid-sync.
		tmp, err := os.CreateTemp(filepath.Dir(localPath), ".sync-*")
		if err != nil {
			rc.Close()
			return fmt.Errorf("agentsdk: SyncDown: tempfile: %w", err)
		}
		if _, err := io.Copy(tmp, rc); err != nil {
			rc.Close()
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("agentsdk: SyncDown: copy %s: %w", fi.Path, err)
		}
		rc.Close()
		if err := tmp.Chmod(0o755); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("agentsdk: SyncDown: chmod %s: %w", tmp.Name(), err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("agentsdk: SyncDown: close %s: %w", tmp.Name(), err)
		}
		if err := os.Rename(tmp.Name(), localPath); err != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("agentsdk: SyncDown: rename %s: %w", localPath, err)
		}
		// Match local mtime to remote so the next compare doesn't see
		// drift just because the local clock is on the wall and the S3
		// timestamp is from when the upload landed.
		if err := os.Chtimes(localPath, fi.LastModified, fi.LastModified); err != nil {
			return fmt.Errorf("agentsdk: SyncDown: chtimes %s: %w", localPath, err)
		}
	}
	return nil
}

// SyncUp is the reverse of SyncDown: walks `localDir` and uploads every
// file to the matching subpath under `prefix`. A file is uploaded iff
// the remote is missing, a different size, or older than the local
// mtime. After upload, the local mtime is matched to the resulting S3
// LastModified so subsequent rounds don't churn.
//
// Last-writer-wins semantics on multi-replica: two replicas concurrently
// uploading the same path will end up with whichever finished last.
// That's correct for self-updates (both replicas converge to the same
// new version anyway). For shared mutable state with concurrent
// writers, use the agent's Postgres schema instead — files are for
// blobs, rows are for shared state.
//
// Trusted: no access check (builder code that constructs paths itself).
func (a *Agent) SyncUp(ctx context.Context, localDir, prefix string) error {
	prefix = strings.TrimRight(prefix, "/")
	if prefix != "" {
		if _, err := normalizePath(prefix); err != nil {
			return fmt.Errorf("agentsdk: SyncUp: prefix: %w", err)
		}
	}
	if localDir == "" {
		return fmt.Errorf("agentsdk: SyncUp: localDir is required")
	}

	return filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("agentsdk: SyncUp: rel: %w", err)
		}
		// filepath.Rel uses the OS separator; storage paths always use '/'.
		remotePath := filepath.ToSlash(rel)
		if prefix != "" {
			remotePath = prefix + "/" + remotePath
		}

		// Skip if remote already matches local.
		if remote, err := a.StatFile(ctx, remotePath); err == nil {
			if remote.Size == info.Size() && !info.ModTime().After(remote.LastModified) {
				return nil
			}
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("agentsdk: SyncUp: open %s: %w", path, err)
		}
		defer f.Close()
		uploaded, err := a.WriteFile(ctx, remotePath, f, syncContentTypeFor(path))
		if err != nil {
			return fmt.Errorf("agentsdk: SyncUp: write %s: %w", remotePath, err)
		}
		// Match local mtime to remote so the next compare doesn't see
		// drift just because the upload landed slightly after the local
		// write.
		if err := os.Chtimes(path, uploaded.LastModified, uploaded.LastModified); err != nil {
			return fmt.Errorf("agentsdk: SyncUp: chtimes %s: %w", path, err)
		}
		return nil
	})
}

// syncContentTypeFor picks an octet-stream default with a few common
// overrides. Builders who care about exact MIME types should use
// WriteFile directly with the right contentType — SyncUp's job is to
// persist bytes, not to serve them.
func syncContentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "application/json"
	case ".txt", ".md":
		return "text/plain"
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}
