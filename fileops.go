package agentsdk

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// scanLineMax caps a single line for the line-oriented file ops (fileGrep/
// fileHead/fileTail/fileLines). A line longer than this fails with a signpost
// toward the byte-window readers, which don't assume line structure.
const scanLineMax = 8 << 20 // 8 MiB

// newFileScanner returns a bufio.Scanner with the enlarged line buffer the
// file ops share.
func newFileScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), scanLineMax)
	return sc
}

func scanErr(err error) error {
	return fmt.Errorf("scan: %w (a line may exceed %d MiB — use fileReadRangeBytes for binary or very long lines)", err, scanLineMax>>20)
}

// readRange returns the inclusive-length byte window [start, start+length) of
// path. Cache-aware: a locally-cached copy is seeked; otherwise a true S3
// Range fetch is issued (readRange never triggers a full spill). The window
// is capped at maxReadFileBytes so it can't blow the goja heap.
func (r *run) readRange(ctx context.Context, path string, start, length int64) ([]byte, error) {
	if start < 0 {
		return nil, fmt.Errorf("start must be >= 0, got %d", start)
	}
	if length <= 0 {
		return nil, fmt.Errorf("length must be > 0, got %d", length)
	}
	if length > maxReadFileBytes {
		return nil, fmt.Errorf("window of %d bytes exceeds the %d MiB cap — read in smaller windows", length, maxReadFileBytes>>20)
	}
	canon, err := normalizePath(path)
	if err != nil {
		return nil, err
	}
	if rc, ok := r.fileCache.open(canon); ok {
		defer rc.Close()
		if seeker, ok := rc.(io.Seeker); ok {
			if _, err := seeker.Seek(start, io.SeekStart); err != nil {
				return nil, err
			}
		} else if _, err := io.CopyN(io.Discard, rc, start); err != nil && err != io.EOF {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(rc, length))
	}
	src, err := r.agent.openFileRangeRaw(ctx, canon, start, start+length-1)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	return io.ReadAll(io.LimitReader(src, length))
}

// grepOpts mirrors the JS opts bag for grep().
type grepOpts struct {
	ignoreCase  bool
	invert      bool
	lineNumbers bool
	max         int // max matching lines emitted (0 → grepDefaultMax)
}

const grepDefaultMax = 1000

// grepFile streams path line by line and returns matching lines joined by
// '\n'. Output is bounded by max lines and maxReadFileBytes; anything past
// the bound is counted and reported in a trailing truncation note.
func (r *run) grepFile(ctx context.Context, path, pattern string, opts grepOpts) (string, error) {
	expr := pattern
	if opts.ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	rc, err := r.openCached(ctx, path)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	max := opts.max
	if max <= 0 {
		max = grepDefaultMax
	}
	sc := newFileScanner(rc)
	var out strings.Builder
	emitted, lineNo, dropped := 0, 0, 0
	for sc.Scan() {
		lineNo++
		hit := re.MatchString(sc.Text())
		if opts.invert {
			hit = !hit
		}
		if !hit {
			continue
		}
		if emitted >= max || out.Len() > maxReadFileBytes {
			dropped++
			continue
		}
		if opts.lineNumbers {
			out.WriteString(strconv.Itoa(lineNo))
			out.WriteByte(':')
		}
		out.WriteString(sc.Text())
		out.WriteByte('\n')
		emitted++
	}
	if err := sc.Err(); err != nil {
		return "", scanErr(err)
	}
	if dropped > 0 {
		fmt.Fprintf(&out, "… (truncated; %d more match(es) — narrow the pattern or raise opts.max)\n", dropped)
	}
	return out.String(), nil
}

// headLines returns the first n lines of path (default 10).
func (r *run) headLines(ctx context.Context, path string, n int) (string, error) {
	if n <= 0 {
		n = 10
	}
	rc, err := r.openCached(ctx, path)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	sc := newFileScanner(rc)
	var out strings.Builder
	for i := 0; i < n && sc.Scan(); i++ {
		out.WriteString(sc.Text())
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", scanErr(err)
	}
	return out.String(), nil
}

// readLineWindow returns `count` lines (default 10) starting at the 1-based
// line `start` (default 1).
func (r *run) readLineWindow(ctx context.Context, path string, start, count int) (string, error) {
	if start < 1 {
		start = 1
	}
	if count <= 0 {
		count = 10
	}
	rc, err := r.openCached(ctx, path)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	sc := newFileScanner(rc)
	var out strings.Builder
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo < start {
			continue
		}
		if lineNo >= start+count {
			break
		}
		out.WriteString(sc.Text())
		out.WriteByte('\n')
		if out.Len() > maxReadFileBytes {
			return "", fmt.Errorf("line window exceeds the %d MiB cap — request fewer lines", maxReadFileBytes>>20)
		}
	}
	if err := sc.Err(); err != nil {
		return "", scanErr(err)
	}
	return out.String(), nil
}

// tailLines returns the last n lines of path (default 10), fetching only a
// trailing window that grows until it holds n full lines (or the whole file,
// or the read cap).
func (r *run) tailLines(ctx context.Context, path string, n int) (string, error) {
	if n <= 0 {
		n = 10
	}
	canon, err := normalizePath(path)
	if err != nil {
		return "", err
	}
	info, err := r.agent.StatFile(ctx, canon)
	if err != nil {
		return "", err
	}
	size := info.Size
	if size <= 0 {
		return "", nil
	}
	window := int64(64 << 10)
	for {
		capped := false
		if window > maxReadFileBytes {
			window = maxReadFileBytes
			capped = true
		}
		if window >= size {
			window = size
		}
		start := size - window
		b, err := r.readRange(ctx, canon, start, window)
		if err != nil {
			return "", err
		}
		parts := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		// A window that starts mid-file truncates its first line — drop it.
		if start != 0 && len(parts) > 0 {
			parts = parts[1:]
		}
		if start == 0 || len(parts) >= n || capped {
			if len(parts) > n {
				parts = parts[len(parts)-n:]
			}
			if len(parts) == 0 {
				return "", nil
			}
			return strings.Join(parts, "\n") + "\n", nil
		}
		window *= 2
	}
}
