package jsexec

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

type bufferTransport struct{ *bytes.Buffer }

func (bufferTransport) Close() error { return nil }

func TestFramed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		size uint32
	}{
		{"oversized", nil, maxFrameBytes + 1},
		{"empty", nil, 0},
		{"truncated", []byte(`{`), 10},
		{"invalid_json", []byte(`xx`), 2},
		{"unknown_field", []byte(`{"type":"done","surprise":1}`), 0},
		{"trailing", []byte(`{"type":"done"} {}`), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			n := tc.size
			if len(tc.body) > 0 && n == 0 {
				n = uint32(len(tc.body))
			}
			_ = binary.Write(&b, binary.BigEndian, n)
			b.Write(tc.body)
			f := framed{rw: bufferTransport{&b}, max: maxFrameBytes}
			if _, err := f.read(); err == nil {
				t.Fatal("malformed frame accepted")
			}
		})
	}
	t.Run("round_trip", func(t *testing.T) {
		f := framed{rw: bufferTransport{new(bytes.Buffer)}, max: 1024}
		want := frame{Type: "call", ID: "abc", Call: 1, Name: "echo", Args: []byte(`[1]`)}
		if err := f.write(want); err != nil {
			t.Fatal(err)
		}
		got, err := f.read()
		if err != nil || got.ID != want.ID || got.Name != want.Name || !bytes.Equal(got.Args, want.Args) {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("short_writes", func(t *testing.T) {
		rw := &shortTransport{Buffer: new(bytes.Buffer)}
		f := framed{rw: rw, max: 1024}
		if err := f.write(frame{Type: "ready"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.read(); err != nil {
			t.Fatal(err)
		}
	})
}

type shortTransport struct{ *bytes.Buffer }

func (s *shortTransport) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return s.Buffer.Write(p)
}
func (s *shortTransport) Close() error { return nil }

var _ io.ReadWriteCloser = (*shortTransport)(nil)
