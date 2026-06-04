package agentsdk

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// encode/decode through the same params the bindings use.
func (r *run) testEncode(ctx context.Context, src, codec, dst string) (*transformResult, error) {
	return r.transformFile(ctx, src, codec, dst, encodeContentType(codec), codecSuffix[codec], textCodecs[codec], encoders[codec])
}

func (r *run) testDecode(ctx context.Context, src, codec, dst string) (*transformResult, error) {
	return r.transformFile(ctx, src, codec, dst, "application/octet-stream", ".bin", false, decoders[codec])
}

func TestCodecRoundTrip(t *testing.T) {
	for _, codec := range []string{"base64", "base64url", "hex", "gzip"} {
		t.Run(codec, func(t *testing.T) {
			_, mock, r := storageAgent(t)
			original := []byte("the quick brown fox\x00\x01\x02 jumps over 13 lazy dogs")
			mock.put("in", original)

			if _, err := r.testEncode(context.Background(), "in", codec, "enc"); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if _, err := r.testDecode(context.Background(), "enc", codec, "dec"); err != nil {
				t.Fatalf("decode: %v", err)
			}
			mock.mu.Lock()
			got := mock.files["dec"]
			mock.mu.Unlock()
			if !bytes.Equal(got, original) {
				t.Fatalf("round trip mismatch: got %q want %q", got, original)
			}
		})
	}
}

func TestEncodeInlineSmallText(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("in", []byte("hello"))

	res, err := r.testEncode(context.Background(), "in", "base64", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !res.inline || res.savedTo != "" {
		t.Fatalf("small text result should be inline; got %+v", res)
	}
	if res.content != "aGVsbG8=" {
		t.Fatalf("got %q", res.content)
	}
}

func TestEncodeSpillsLargeText(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("in", bytes.Repeat([]byte("x"), 20*1024)) // base64 > 8 KiB inline threshold

	res, err := r.testEncode(context.Background(), "in", "base64", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if res.inline || res.savedTo == "" {
		t.Fatalf("large text result should spill; got %+v", res)
	}
	if !strings.HasPrefix(res.savedTo, reservedTmpPath+"/") {
		t.Fatalf("spill should land in scratch; got %q", res.savedTo)
	}
	if res.preview == "" {
		t.Fatal("spilled result should carry a preview")
	}
}

func TestBinaryOutputNeverInline(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("in", []byte("aGVsbG8=")) // base64 of "hello"

	res, err := r.testDecode(context.Background(), "in", "base64", "")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.inline || res.savedTo == "" {
		t.Fatalf("binary decode output must always be a file; got %+v", res)
	}
}

func TestDstEqualsSrcRejected(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("same", []byte("data"))

	_, err := r.testEncode(context.Background(), "same", "base64", "same")
	if err == nil || !strings.Contains(err.Error(), "overwrite the source") {
		t.Fatalf("expected dst==src rejection, got %v", err)
	}
}

func TestDecodeTextFileLatin1(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("in", []byte{0x48, 0x69, 0xE9}) // "Hi" + é in latin1

	fn, err := lookupCharset("latin1")
	if err != nil {
		t.Fatalf("lookupCharset: %v", err)
	}
	if _, err := r.transformFile(context.Background(), "in", "latin1", "out", "text/plain; charset=utf-8", ".txt", true, fn); err != nil {
		t.Fatalf("decodeText: %v", err)
	}
	mock.mu.Lock()
	got := mock.files["out"]
	mock.mu.Unlock()
	if string(got) != "Hié" {
		t.Fatalf("got %q, want %q", got, "Hié")
	}
}

func TestUnknownCharset(t *testing.T) {
	if _, err := lookupCharset("klingon"); err == nil {
		t.Fatal("expected an error for an unknown charset")
	}
}
