package agentsdk

import (
	"bytes"
	"context"
	"io"
)

const (
	// spillInlineThreshold is the size below which a response body is returned
	// inline to the JS VM rather than spilled to storage. Matches httpRequest's
	// httpAutoSaveThreshold so HTTP bindings share one overflow boundary.
	spillInlineThreshold = 8 * 1024

	// spillPreviewBytes is the head of the body kept as bodyPreview after spill
	// so the LLM can inspect its shape without a follow-up fileRead.
	spillPreviewBytes = 1024
)

// peekAndSpill reads up to spillInlineThreshold+1 bytes from r. If the total
// fits in the threshold, returns the bytes inline (savedTo=""). Otherwise
// opens an agent storage write at dstPath and streams the remainder in a
// single pass (peek + rest via io.MultiReader), returning a 1 KiB preview +
// savedTo + total size.
//
// On any error, peekAndSpill drains the rest of r into io.Discard before
// returning so the underlying transport isn't wedged by unread bytes.
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
// each other.
func newCallID() string {
	return randomHex(4)
}
