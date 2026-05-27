package agentsdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dop251/goja"
)

// recordingStorageServer stands in for airlock's /api/agent/storage PUT
// endpoint. It records each path and the full payload so the test can
// assert on what was streamed.
type recordingStorageServer struct {
	mu     sync.Mutex
	writes map[string][]byte
	srv    *httptest.Server
}

func newRecordingStorageServer(t *testing.T) *recordingStorageServer {
	t.Helper()
	r := &recordingStorageServer{writes: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "PUT" || !strings.HasPrefix(req.URL.Path, "/api/agent/storage/") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		path := strings.TrimPrefix(req.URL.Path, "/api/agent/storage/")
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.mu.Lock()
		r.writes[path] = body
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recordingStorageServer) agent() *Agent {
	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(r.srv.URL, "tok", a.httpClient)
	return a
}

func (r *recordingStorageServer) writeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.writes)
}

func (r *recordingStorageServer) bodyAt(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes[path]
}

func TestPeekAndSpill_InlineWhenUnderThreshold(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"tiny", 100},
		{"half", spillInlineThreshold / 2},
		{"exact_threshold", spillInlineThreshold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRecordingStorageServer(t)
			agent := srv.agent()
			payload := bytes.Repeat([]byte("a"), tt.size)
			inline, savedTo, size, err := peekAndSpill(
				context.Background(), agent, bytes.NewReader(payload),
				"tmp/conn-x-deadbeef.bin", "text/plain")
			if err != nil {
				t.Fatalf("peekAndSpill: %v", err)
			}
			if savedTo != "" {
				t.Errorf("savedTo = %q, want empty (input below threshold)", savedTo)
			}
			if size != int64(tt.size) {
				t.Errorf("size = %d, want %d", size, tt.size)
			}
			if !bytes.Equal(inline, payload) {
				t.Errorf("inline = %d bytes, want full %d-byte payload", len(inline), tt.size)
			}
			if srv.writeCount() != 0 {
				t.Errorf("storage endpoint hit %d times; want 0 for inline result", srv.writeCount())
			}
		})
	}
}

func TestPeekAndSpill_OverflowSpillsAndReturnsPreview(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"just_over_threshold", spillInlineThreshold + 1},
		{"large", 100 * 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newRecordingStorageServer(t)
			agent := srv.agent()
			payload := make([]byte, tt.size)
			for i := range payload {
				payload[i] = byte('a' + (i % 26))
			}
			dst := "tmp/conn-x-deadbeef.bin"
			inline, savedTo, size, err := peekAndSpill(
				context.Background(), agent, bytes.NewReader(payload),
				dst, "application/octet-stream")
			if err != nil {
				t.Fatalf("peekAndSpill: %v", err)
			}
			if savedTo != dst {
				t.Errorf("savedTo = %q, want %q", savedTo, dst)
			}
			if size != int64(tt.size) {
				t.Errorf("size = %d, want %d", size, tt.size)
			}
			if len(inline) != spillPreviewBytes {
				t.Errorf("preview = %d bytes, want %d", len(inline), spillPreviewBytes)
			}
			if !bytes.Equal(inline, payload[:spillPreviewBytes]) {
				t.Errorf("preview bytes don't match head of payload")
			}
			stored := srv.bodyAt(dst)
			if !bytes.Equal(stored, payload) {
				t.Errorf("stored body length = %d, want %d (and bytes should match)", len(stored), tt.size)
			}
		})
	}
}

type errReader struct {
	n   int
	err error
	buf []byte
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, e.err
	}
	if e.n < len(p) {
		p = p[:e.n]
	}
	copy(p, e.buf[:len(p)])
	e.n -= len(p)
	return len(p), nil
}

func TestPeekAndSpill_ReadErrorPropagates(t *testing.T) {
	srv := newRecordingStorageServer(t)
	agent := srv.agent()
	bad := &errReader{n: 32, err: errors.New("boom"), buf: bytes.Repeat([]byte("x"), 32)}
	_, _, _, err := peekAndSpill(context.Background(), agent, bad,
		"tmp/whatever.bin", "application/octet-stream")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	if srv.writeCount() != 0 {
		t.Errorf("storage hit %d times on read-error path; want 0", srv.writeCount())
	}
}

func TestNewCallID_Length(t *testing.T) {
	id := newCallID()
	if len(id) != 8 {
		t.Errorf("newCallID() = %q (len %d), want 8 hex chars", id, len(id))
	}
}

func TestSetStreamFields_InlineVsSpilled(t *testing.T) {
	vm := goja.New()

	t.Run("inline", func(t *testing.T) {
		obj := vm.NewObject()
		setStreamFields(obj, "stdout", spillFields{inline: []byte("hello"), size: 5})
		if got := obj.Get("stdout").String(); got != "hello" {
			t.Errorf("stdout = %q, want hello", got)
		}
		if v := obj.Get("stdoutSavedTo"); v != nil && !goja.IsUndefined(v) {
			t.Errorf("stdoutSavedTo should be absent on inline result, got %v", v)
		}
	})

	t.Run("spilled", func(t *testing.T) {
		obj := vm.NewObject()
		setStreamFields(obj, "stdout", spillFields{
			inline: []byte("preview…"), savedTo: "tmp/exec-x-deadbeef-stdout.bin", size: 50000,
		})
		if v := obj.Get("stdout"); v != nil && !goja.IsUndefined(v) {
			t.Errorf("stdout should be absent on spilled result, got %v", v)
		}
		if got := obj.Get("stdoutPreview").String(); got != "preview…" {
			t.Errorf("stdoutPreview = %q", got)
		}
		if got := obj.Get("stdoutSavedTo").String(); got != "tmp/exec-x-deadbeef-stdout.bin" {
			t.Errorf("stdoutSavedTo = %q", got)
		}
		if got := obj.Get("stdoutSize").ToInteger(); got != 50000 {
			t.Errorf("stdoutSize = %d, want 50000", got)
		}
	})
}

func TestExecOverflowNote_MentionsCorrectStreams(t *testing.T) {
	tests := []struct {
		name           string
		out, err       spillFields
		mustContainAll []string
		mustNotContain []string
	}{
		{
			name:           "stdout only",
			out:            spillFields{savedTo: "tmp/exec-x-id-stdout.bin", size: 9000},
			err:            spillFields{}, // inline
			mustContainAll: []string{"stdout", "9000", "tmp/exec-x-id-stdout.bin"},
			mustNotContain: []string{"stderr"},
		},
		{
			name:           "stderr only",
			out:            spillFields{},
			err:            spillFields{savedTo: "tmp/exec-x-id-stderr.bin", size: 9000},
			mustContainAll: []string{"stderr", "9000", "tmp/exec-x-id-stderr.bin"},
			mustNotContain: []string{"stdout"},
		},
		{
			name: "both",
			out:  spillFields{savedTo: "tmp/exec-x-id-stdout.bin", size: 11},
			err:  spillFields{savedTo: "tmp/exec-x-id-stderr.bin", size: 22},
			mustContainAll: []string{
				"stdout", "stderr",
				"tmp/exec-x-id-stdout.bin", "tmp/exec-x-id-stderr.bin",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note := execOverflowNote(tt.out, tt.err)
			for _, s := range tt.mustContainAll {
				if !strings.Contains(note, s) {
					t.Errorf("note=%q missing %q", note, s)
				}
			}
			for _, s := range tt.mustNotContain {
				if strings.Contains(note, s) {
					t.Errorf("note=%q must not mention %q", note, s)
				}
			}
		})
	}
}
