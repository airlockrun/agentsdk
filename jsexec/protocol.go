package jsexec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

const maxFrameBytes = 1 << 20

// Frames use a four-byte big-endian length followed by UTF-8 JSON. Attach must
// be non-TTY and demultiplex Docker stdout before constructing a Client.
type frame struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Call      uint64          `json:"call,omitempty"`
	Code      string          `json:"code,omitempty"`
	Options   *Options        `json:"options,omitempty"`
	Name      string          `json:"name,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Result    *Result         `json:"result,omitempty"`
	Exception *Exception      `json:"exception,omitempty"`
	Error     string          `json:"error,omitempty"`
	Terminal  bool            `json:"terminal,omitempty"`
}

type framed struct {
	rw  io.ReadWriteCloser
	max int
	mu  sync.Mutex
}

func (f *framed) read() (frame, error) {
	var msg frame
	var header [4]byte
	if _, err := io.ReadFull(f.rw, header[:]); err != nil {
		return msg, err
	}
	n := int(binary.BigEndian.Uint32(header[:]))
	if n < 2 || n > f.max {
		return msg, errors.New("jsexec: invalid frame size")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(f.rw, b); err != nil {
		return msg, err
	}
	if !utf8.Valid(b) {
		return msg, errors.New("jsexec: frame is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&msg); err != nil {
		return msg, fmt.Errorf("jsexec: invalid frame: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return msg, errors.New("jsexec: trailing frame data")
	}
	return msg, nil
}

func (f *framed) write(msg frame) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(b) > f.max {
		return errors.New("jsexec: frame too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(b)))
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, part := range [][]byte{header[:], b} {
		for len(part) > 0 {
			n, err := f.rw.Write(part)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}

type received struct {
	frame frame
	err   error
}

func receive(ctx context.Context, f *framed) <-chan received {
	ch := make(chan received)
	go func() {
		for {
			m, e := f.read()
			select {
			case ch <- received{m, e}:
			case <-ctx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	return ch
}

func validResult(r *Result, l Limits) bool {
	if r == nil || r.Undefined && len(r.Output) != 0 || !r.Undefined && !json.Valid(r.Output) || len(r.Output) > l.OutputBytes || len(r.Logs) > l.Logs {
		return false
	}
	n := 0
	for _, log := range r.Logs {
		if log.Level != LogInfo && log.Level != LogWarn && log.Level != LogError {
			return false
		}
		n += len(log.Message)
	}
	return n <= l.LogBytes
}

func validException(e *Exception, l Limits) bool {
	return e == nil || len(e.Name)+len(e.Message) <= l.OutputBytes
}

func allowedCall(m frame, names map[string]bool) bool {
	args := bytes.TrimSpace(m.Args)
	return m.Type == "call" && m.Call > 0 && names[m.Name] && len(args) > 0 && args[0] == '[' && json.Valid(args)
}
