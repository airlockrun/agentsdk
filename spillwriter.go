package agentsdk

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/dop251/goja"
)

const (
	// spillInlineThreshold is the size below which a response body (or
	// stdout/stderr stream) is returned inline to the JS VM rather than
	// spilled to storage. Matches httpRequest's httpAutoSaveThreshold so
	// the LLM sees one consistent inline/overflow boundary across the
	// httpRequest, conn_{slug}, and exec_{slug} bindings.
	spillInlineThreshold = 8 * 1024

	// spillPreviewBytes is the head of the body kept as bodyPreview /
	// stdoutPreview / stderrPreview after spill so the LLM can sniff
	// content type / shape without a follow-up readFile.
	spillPreviewBytes = 1024
)

// peekAndSpill reads up to spillInlineThreshold+1 bytes from r. If the total
// fits in the threshold, returns the bytes inline (savedTo=""). Otherwise
// opens an agent storage write at dstPath and streams the remainder in a
// single pass (peek + rest via io.MultiReader), returning a 1 KiB preview +
// savedTo + total size.
//
// On any error, peekAndSpill drains the rest of r into io.Discard before
// returning so the underlying transport (HTTP body / SSH session pipe) isn't
// wedged by unread bytes.
func peekAndSpill(
	ctx context.Context,
	agent *Agent,
	r io.Reader,
	dstPath string,
	contentType string,
) (inline []byte, savedTo string, size int64, err error) {
	peek, readErr := io.ReadAll(io.LimitReader(r, int64(spillInlineThreshold)+1))
	if readErr != nil {
		_, _ = io.Copy(io.Discard, r)
		return nil, "", 0, readErr
	}
	if len(peek) <= spillInlineThreshold {
		return peek, "", int64(len(peek)), nil
	}

	cr := &spillCountingReader{r: io.MultiReader(bytes.NewReader(peek), r)}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := agent.writeFileRaw(ctx, dstPath, cr, contentType, ""); err != nil {
		_, _ = io.Copy(io.Discard, r)
		return nil, "", 0, err
	}

	previewLen := spillPreviewBytes
	if previewLen > len(peek) {
		previewLen = len(peek)
	}
	return peek[:previewLen], dstPath, cr.n, nil
}

// spillCountingReader tallies bytes read so a streamed write can report
// the final size without buffering the whole body.
type spillCountingReader struct {
	r io.Reader
	n int64
}

func (c *spillCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// newCallID returns an 8-char hex id used to label spill files. Per call,
// not per run, so successive bindings within the same run don't overwrite
// each other and the two halves of an exec call (stdout + stderr) share
// the same id.
func newCallID() string {
	return randomHex(4)
}

// spillFields is the result shape both stdout and stderr of an exec call
// reduce to before being projected onto the JS envelope.
type spillFields struct {
	inline  []byte
	savedTo string
	size    int64
	err     error
}

// spillFor reads r through peekAndSpill, packaging the result. Used by the
// exec_{slug}.run JS binding which drains stdout and stderr concurrently.
func spillFor(ctx context.Context, agent *Agent, r io.Reader, dstPath string) spillFields {
	inline, savedTo, size, err := peekAndSpill(ctx, agent, r, dstPath, "application/octet-stream")
	return spillFields{inline: inline, savedTo: savedTo, size: size, err: err}
}

// setStreamFields projects a spillFields onto a JS envelope. When inline
// (savedTo==""), sets `{name}: "<bytes>"`. When spilled, sets
// `{name}Preview`, `{name}SavedTo`, and `{name}Size`. Keys are mutually
// exclusive — never both — so the LLM's `if (resp.stdoutSavedTo)` idiom
// reads cleanly.
func setStreamFields(obj *goja.Object, name string, s spillFields) {
	if s.savedTo == "" {
		obj.Set(name, string(s.inline))
		return
	}
	obj.Set(name+"Preview", string(s.inline))
	obj.Set(name+"SavedTo", s.savedTo)
	obj.Set(name+"Size", s.size)
}

// execOverflowNote is the LLM-readable explanation appended when at least
// one of stdout/stderr was spilled.
func execOverflowNote(outR, errR spillFields) string {
	switch {
	case outR.savedTo != "" && errR.savedTo != "":
		return fmt.Sprintf("stdout (%d bytes) and stderr (%d bytes) exceeded inline threshold; saved to %s and %s. Use readFile to read.", outR.size, errR.size, outR.savedTo, errR.savedTo)
	case outR.savedTo != "":
		return fmt.Sprintf("stdout (%d bytes) exceeded inline threshold; saved to %s. Use readFile(stdoutSavedTo) to read.", outR.size, outR.savedTo)
	default:
		return fmt.Sprintf("stderr (%d bytes) exceeded inline threshold; saved to %s. Use readFile(stderrSavedTo) to read.", errR.size, errR.savedTo)
	}
}
