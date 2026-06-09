package agentsdk

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/dop251/goja"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// streamFunc transforms src into dst in a single streaming pass — the shared
// shape behind every file→file codec, so the read-cache + spill plumbing is
// written once and the only thing that varies is the byte transform.
type streamFunc func(dst io.Writer, src io.Reader) error

// encoders turn raw bytes into a wire form; decoders reverse them. base64/hex
// produce ASCII text (textCodecs below); gzip is binary either way.
var encoders = map[string]streamFunc{
	"base64": func(dst io.Writer, src io.Reader) error {
		return copyClose(base64.NewEncoder(base64.StdEncoding, dst), src)
	},
	"base64url": func(dst io.Writer, src io.Reader) error {
		return copyClose(base64.NewEncoder(base64.URLEncoding, dst), src)
	},
	"hex": func(dst io.Writer, src io.Reader) error {
		_, err := io.Copy(hex.NewEncoder(dst), src)
		return err
	},
	"gzip": func(dst io.Writer, src io.Reader) error { return copyClose(gzip.NewWriter(dst), src) },
}

var decoders = map[string]streamFunc{
	"base64": func(dst io.Writer, src io.Reader) error {
		_, err := io.Copy(dst, base64.NewDecoder(base64.StdEncoding, src))
		return err
	},
	"base64url": func(dst io.Writer, src io.Reader) error {
		_, err := io.Copy(dst, base64.NewDecoder(base64.URLEncoding, src))
		return err
	},
	"hex": func(dst io.Writer, src io.Reader) error { _, err := io.Copy(dst, hex.NewDecoder(src)); return err },
	"gzip": func(dst io.Writer, src io.Reader) error {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return err
		}
		defer zr.Close()
		_, err = io.Copy(dst, zr)
		return err
	},
}

// textCodecs are the codecs whose ENCODE output is ASCII text (so a small
// result can ride back inline). Decode output is always treated as binary.
var textCodecs = map[string]bool{"base64": true, "base64url": true, "hex": true}

var codecSuffix = map[string]string{"base64": ".b64", "base64url": ".b64u", "hex": ".hex", "gzip": ".gz"}

// charsets maps a charset name to its decoder source encoding. The decode
// target is always UTF-8.
var charsets = map[string]encoding.Encoding{
	"latin1":       charmap.ISO8859_1,
	"iso-8859-1":   charmap.ISO8859_1,
	"windows-1252": charmap.Windows1252,
	"cp1252":       charmap.Windows1252,
	"utf-16":       unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
	"utf-16le":     unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"utf-16be":     unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
}

// copyClose streams src into a WriteCloser and closes it, surfacing whichever
// error comes first. Used by codecs (base64/gzip encoders) that must be closed
// to flush their final block.
func copyClose(wc io.WriteCloser, src io.Reader) error {
	if _, err := io.Copy(wc, src); err != nil {
		wc.Close()
		return err
	}
	return wc.Close()
}

// transformResult is the file→file transform outcome projected to JS.
type transformResult struct {
	inline  bool
	content string // set when inline
	savedTo string // canonical storage path of the output (when not inline)
	preview string // short text sniff for a spilled text output
	size    int64
}

// transformFile reads src (cache-aware), streams it through fn, and lands the
// output. With dst set the output is written there (streamed). Without dst the
// output goes to an auto scratch path: a small text result rides back inline,
// otherwise it spills with a preview. dst must differ from src.
func (r *run) transformFile(ctx context.Context, src, codecName, dst, contentType, suffix string, textOutput bool, fn streamFunc) (*transformResult, error) {
	srcCanon, err := normalizePath(src)
	if err != nil {
		return nil, err
	}
	var dstCanon string
	if dst != "" {
		dstCanon, err = normalizePath(dst)
		if err != nil {
			return nil, err
		}
		if dstCanon == srcCanon {
			return nil, errors.New("transforms can't overwrite the source; pick a different dst")
		}
	}

	return r.streamThrough(ctx, src, dstCanon, contentType, suffix, textOutput, fn)
}

// streamThrough reads src (cache-aware), pipes it through fn on a goroutine,
// and lands the output: dstCanon set → streamed write there (+cache
// invalidate); empty → an auto scratch path that rides back inline when the
// result is small text, else spills with a preview. The caller owns the
// src≠dst policy (codecs reject it, edits allow in-place).
func (r *run) streamThrough(ctx context.Context, src, dstCanon, contentType, suffix string, textOutput bool, fn streamFunc) (*transformResult, error) {
	in, err := r.openCached(ctx, src)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer in.Close()
		pw.CloseWithError(fn(pw, in))
	}()

	if dstCanon != "" {
		cr := &spillCountingReader{r: pr}
		werr := r.agent.writeFileRaw(ctx, dstCanon, cr, contentType, "")
		io.Copy(io.Discard, pr) // drain remainder so the goroutine can't leak on an early write error
		if werr != nil {
			return nil, werr
		}
		r.invalidateCache(dstCanon)
		return &transformResult{savedTo: dstCanon, size: cr.n}, nil
	}

	autoPath := reservedTmpPath + "/" + newCallID() + suffix
	if textOutput {
		inline, savedTo, size, err := peekAndSpill(ctx, r.agent, pr, autoPath, contentType)
		if err != nil {
			return nil, err
		}
		if savedTo == "" {
			return &transformResult{inline: true, content: string(inline), size: size}, nil
		}
		return &transformResult{savedTo: savedTo, size: size, preview: string(inline)}, nil
	}
	// Binary output never rides inline (it'd corrupt as a JS string).
	cr := &spillCountingReader{r: pr}
	werr := r.agent.writeFileRaw(ctx, autoPath, cr, contentType, "")
	io.Copy(io.Discard, pr)
	if werr != nil {
		return nil, werr
	}
	return &transformResult{savedTo: autoPath, size: cr.n}, nil
}

// toMap projects transformResult onto the LLM-facing JSON shape shared by
// the run_js fileEncode/fileDecode/fileDecodeText/fileEditLines/fileSed
// bindings and the direct-mode equivalents. `inline:true` carries `content`;
// `inline:false` carries `savedTo` and (when set) `preview`.
func (r *transformResult) toMap() map[string]any {
	out := map[string]any{
		"inline": r.inline,
		"size":   r.size,
	}
	if r.inline {
		out["content"] = r.content
		return out
	}
	out["savedTo"] = r.savedTo
	if r.preview != "" {
		out["preview"] = r.preview
	}
	return out
}

func transformResultToJS(vm *goja.Runtime, res *transformResult) goja.Value {
	return vm.ToValue(res.toMap())
}

// encodeContentType returns the MIME type for an encode codec's output.
func encodeContentType(codecName string) string {
	if codecName == "gzip" {
		return "application/gzip"
	}
	return "text/plain; charset=utf-8"
}

// transformArgs parses the common (src, codec|charset, dst?) argument shape
// shared by encodeFile/decodeFile/decodeTextFile. Panics (as a JS throw) on a
// missing src or codec, mirroring the other bindings.
func transformArgs(vm *goja.Runtime, call goja.FunctionCall, name string) (src, spec, dst string) {
	src, err := pathArg(call.Argument(0))
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("%s: %w", name, err)))
	}
	specArg := call.Argument(1)
	if goja.IsUndefined(specArg) || goja.IsNull(specArg) {
		panic(vm.NewGoError(fmt.Errorf("%s: codec/charset is required", name)))
	}
	spec = specArg.String()
	if d := call.Argument(2); !goja.IsUndefined(d) && !goja.IsNull(d) {
		dst, err = pathArg(d)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("%s: dst: %w", name, err)))
		}
	}
	return src, spec, dst
}

// optPathArg parses an optional dst path argument; returns "" when absent.
func optPathArg(vm *goja.Runtime, v goja.Value, name string) string {
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	p, err := pathArg(v)
	if err != nil {
		panic(vm.NewGoError(fmt.Errorf("%s: dst: %w", name, err)))
	}
	return p
}

// checkTransformAccess gates a transform's src read and (when given) dst write.
// An omitted dst targets the framework scratch dir, which the authed gate
// already covers, so it needs no check.
func checkTransformAccess(ctx context.Context, agent *Agent, vm *goja.Runtime, name, src, dst string) {
	if err := agent.CheckFileAccess(ctx, src, OpRead); err != nil {
		panic(vm.NewGoError(fmt.Errorf("%s: %w", name, err)))
	}
	if dst != "" {
		if err := agent.CheckFileAccess(ctx, dst, OpWrite); err != nil {
			panic(vm.NewGoError(fmt.Errorf("%s: %w", name, err)))
		}
	}
}

// lookupCharset resolves a charset name to its decoder streamFunc (→ UTF-8).
func lookupCharset(name string) (streamFunc, error) {
	enc, ok := charsets[name]
	if !ok {
		return nil, fmt.Errorf("unknown charset %q (supported: latin1, iso-8859-1, windows-1252, cp1252, utf-16, utf-16le, utf-16be)", name)
	}
	return func(dst io.Writer, src io.Reader) error {
		_, err := io.Copy(dst, transform.NewReader(src, enc.NewDecoder()))
		return err
	}, nil
}
