package agentsdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

type generatedUpload struct {
	left    int64
	read    atomic.Int64
	eof     atomic.Bool
	maxRead atomic.Int64
}

func (r *generatedUpload) Read(p []byte) (int, error) {
	if r.left == 0 {
		r.eof.Store(true)
		return 0, io.EOF
	}
	if int64(len(p)) > r.maxRead.Load() {
		r.maxRead.Store(int64(len(p)))
	}
	n := int(min(int64(len(p)), r.left))
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.left -= int64(n)
	r.read.Add(int64(n))
	return n, nil
}

func TestWriteFileStreamsUnknownLength(t *testing.T) {
	const size = 64<<20 + 123
	source := &generatedUpload{left: size}
	want := sha256.New()
	_, _ = io.Copy(want, io.LimitReader(&generatedUpload{left: size}, size))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/agent/storage/docs/data" || r.Header.Get("Content-Type") != "text/plain" || r.Header.Get("Authorization") != "Bearer token" || r.ContentLength != -1 {
			t.Errorf("request = %s %s %v length=%d", r.Method, r.URL.Path, r.Header, r.ContentLength)
		}
		var first [1024]byte
		if _, err := io.ReadFull(r.Body, first[:]); err != nil {
			t.Error(err)
			return
		}
		if source.eof.Load() {
			t.Error("source fully read before HTTP ingress")
		}
		hash := sha256.New()
		_, _ = hash.Write(first[:])
		n, err := io.Copy(hash, r.Body)
		if err != nil || n+int64(len(first)) != size || string(hash.Sum(nil)) != string(want.Sum(nil)) {
			t.Errorf("integrity: n=%d err=%v", n, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	a := newAgentRegistrationState(Config{Description: "stream test"})
	a.phase = agentRunning
	a.client = newAirlockClient(server.URL, "token", server.Client())
	a.RegisterDirectory("docs", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Documents"})
	before := time.Now()
	info, err := a.WriteFile(t.Context(), "docs/data", source, "text/plain")
	if err != nil || info.Size != size || info.Path != "docs/data" || info.Filename != "data" || info.ContentType != "text/plain" || info.LastModified.Before(before) {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if source.maxRead.Load() > 64<<10 {
		t.Fatalf("unbounded source read: %d", source.maxRead.Load())
	}
}

func TestWriteFileKnownAndEmptyReaders(t *testing.T) {
	for _, data := range []string{"hello", ""} {
		t.Run(data, func(t *testing.T) {
			a, _ := testAgent(t)
			info, err := a.WriteFile(t.Context(), "tmp/data", strings.NewReader(data), "text/plain")
			if err != nil || info.Size != int64(len(data)) {
				t.Fatalf("info=%+v err=%v", info, err)
			}
		})
	}
}

func TestWriteFileDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var redirected atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
			}))
			defer server.Close()
			a, _ := testAgent(t)
			a.client = newAirlockClient(server.URL, "token", server.Client())
			if _, err := a.WriteFile(t.Context(), "tmp/data", strings.NewReader("data"), "text/plain"); err == nil {
				t.Fatal("redirect reported a committed upload")
			}
			if redirected.Load() != 0 {
				t.Fatal("upload followed a redirect")
			}
		})
	}
}

type cancelingUploadReader struct{ cancel context.CancelFunc }

func (r cancelingUploadReader) Read([]byte) (int, error) { r.cancel(); return 0, context.Canceled }

func TestWriteFileCancelsDuringStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	a, _ := testAgent(t)
	a.client = newAirlockClient(server.URL, "token", server.Client())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := io.MultiReader(&generatedUpload{left: 2 << 20}, cancelingUploadReader{cancel})
	if _, err := a.WriteFile(ctx, "tmp/data", source, "text/plain"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

type failingUploadReader struct{ err error }

func (r failingUploadReader) Read([]byte) (int, error) { return 0, r.err }

func TestWriteFileFailures(t *testing.T) {
	sourceErr := errors.New("source failed")
	for _, tc := range []struct {
		name     string
		source   io.Reader
		status   int
		consume  bool
		want     error
		canceled bool
	}{
		{"source error", failingUploadReader{sourceErr}, 204, true, sourceErr, false},
		{"premature EOF", failingUploadReader{io.ErrUnexpectedEOF}, 204, true, io.ErrUnexpectedEOF, false},
		{"early success", strings.NewReader("not consumed"), 204, false, io.ErrUnexpectedEOF, false},
		{"backend error", strings.NewReader("data"), 500, true, nil, false},
		{"cancel", panicReader{}, 204, false, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := testAgent(t)
			var responseRead atomic.Int64
			a.client.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.GetBody != nil {
					t.Error("upload must not be replayable")
				}
				if tc.consume {
					_, _ = io.Copy(io.Discard, req.Body)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(&observedResponse{read: &responseRead})}, nil
			})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			_, err := a.WriteFile(ctx, "tmp/data", tc.source, "text/plain")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
			if responseRead.Load() > 64<<10 {
				t.Fatalf("unbounded error response: %d", responseRead.Load())
			}
		})
	}
}

type observedResponse struct{ read *atomic.Int64 }

func (r *observedResponse) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.read.Add(int64(len(p)))
	return len(p), nil
}

func TestFileRefAndManualIndex(t *testing.T) {
	ref := FileRef{ID: "entry", ContentID: "content", Name: "report.txt", Path: "/apps/app/docs/report.txt", ContentType: "text/plain", Size: 6}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/storage/ref", func(w http.ResponseWriter, r *http.Request) {
		var request wire.StorageFileRefRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Path != "docs/report.txt" {
			t.Errorf("ref request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(ref)
	})
	mux.HandleFunc("POST /api/agent/storage/index", func(w http.ResponseWriter, r *http.Request) {
		var request wire.StorageSetIndexRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.File != ref || request.Text != "quarterly revenue" {
			t.Errorf("index request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(ref)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	agent := newAgentRegistrationState(Config{Description: "test agent"})
	agent.phase = agentRunning
	agent.client = newAirlockClient(server.URL, "token", server.Client())
	agent.RegisterDirectory("docs", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Private documents"})

	got, err := agent.StatFileRef(context.Background(), "docs/report.txt")
	if err != nil || got != ref {
		t.Fatalf("StatFileRef() = %+v, %v", got, err)
	}
	got, err = agent.SetFileIndex(context.Background(), got, "quarterly revenue")
	if err != nil || got != ref {
		t.Fatalf("SetFileIndex() = %+v, %v", got, err)
	}
}

func TestTrustedStorageRequiresRegisteredDirectoryBeforeIO(t *testing.T) {
	a, mock := testAgent(t)
	before := len(mock.Requests())

	_, err := a.WriteFile(t.Context(), "sources/main.go", panicReader{}, "text/plain")
	if !errors.Is(err, ErrDirectoryNotRegistered) {
		t.Fatalf("WriteFile() error = %v, want ErrDirectoryNotRegistered", err)
	}
	if got := len(mock.Requests()); got != before {
		t.Fatalf("requests = %d, want %d", got, before)
	}
}

func TestTrustedStorageRegistrationCoverage(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterDirectory("sources", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Generated source files"})
	a.RegisterDirectory("reports", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser, Description: "Reports"})
	a.RegisterDirectory("reports/private", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Private reports"})
	a.RegisterDirectory("users", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser, Scope: ScopeUser, Description: "User files"})

	if _, err := a.WriteFile(context.Background(), "sources/main.go", strings.NewReader("package main"), "text/plain"); err != nil {
		t.Fatalf("internal WriteFile() error = %v", err)
	}
	run := newRun(a, "run-current", "user-owner", "conv-current", context.Background())
	if _, err := a.WriteFile(run.checkedCtx(), "users/user-owner/file.txt", strings.NewReader("data"), "text/plain"); err != nil {
		t.Fatalf("physical scoped WriteFile() error = %v", err)
	}
	if _, err := a.WriteFile(context.Background(), "reports/private/q1.txt", strings.NewReader("data"), "text/plain"); err != nil {
		t.Fatalf("nested WriteFile() error = %v", err)
	}
	if _, err := a.WriteFile(context.Background(), "reports", panicReader{}, "text/plain"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("directory WriteFile() error = %v, want ErrInvalidPath", err)
	}
	if _, err := a.WriteFile(context.Background(), "reporting/q1.txt", panicReader{}, "text/plain"); !errors.Is(err, ErrDirectoryNotRegistered) {
		t.Fatalf("partial-prefix WriteFile() error = %v, want ErrDirectoryNotRegistered", err)
	}

	mock.Reset()
	if _, err := a.ListDir(context.Background(), "", ListOpts{}); err != nil {
		t.Fatalf("ListDir(root) error = %v", err)
	}
	// tmp, sources, reports, and users are roots; reports/private is nested.
	if got := len(mock.Requests()); got != 4 {
		t.Fatalf("root list requests = %d, want 4", got)
	}
}

func TestTrustedStorageOperationsRejectUnregisteredPaths(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterDirectory("registered", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Registered files"})

	tests := []struct {
		name string
		call func() error
	}{
		{name: "open", call: func() error { _, err := a.OpenFile(t.Context(), "unknown/file"); return err }},
		{name: "open range", call: func() error { _, err := a.OpenFileRange(t.Context(), "unknown/file", 0, 1); return err }},
		{name: "read", call: func() error { _, err := a.ReadFile(t.Context(), "unknown/file"); return err }},
		{name: "read range", call: func() error { _, err := a.ReadRange(t.Context(), "unknown/file", 0, 1); return err }},
		{name: "stat", call: func() error { _, err := a.StatFile(t.Context(), "unknown/file"); return err }},
		{name: "stat ref", call: func() error { _, err := a.StatFileRef(t.Context(), "unknown/file"); return err }},
		{name: "list", call: func() error { _, err := a.ListDir(t.Context(), "unknown", ListOpts{}); return err }},
		{name: "delete", call: func() error { return a.DeleteFile(t.Context(), "unknown/file") }},
		{name: "share", call: func() error { _, err := a.ShareFileURL(t.Context(), "unknown/file", 0); return err }},
		{name: "copy destination first", call: func() error { return a.CopyFile(t.Context(), "unknown/source", "missing/destination") }},
		{name: "sync down", call: func() error { return a.SyncDown(t.Context(), "unknown", t.TempDir()+"/new") }},
		{name: "sync up", call: func() error { return a.SyncUp(t.Context(), t.TempDir()+"/missing", "unknown") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock.Reset()
			err := tt.call()
			if !errors.Is(err, ErrDirectoryNotRegistered) {
				t.Fatalf("error = %v, want ErrDirectoryNotRegistered", err)
			}
			if len(mock.Requests()) != 0 {
				t.Fatalf("operation performed HTTP requests: %+v", mock.Requests())
			}
			if tt.name == "copy destination first" && !strings.Contains(err.Error(), "missing/destination") {
				t.Fatalf("copy error = %v, want destination path", err)
			}
		})
	}
}

func TestRegisteredMissingFileRemainsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	a := newAgentRegistrationState(Config{Description: "test agent"})
	a.phase = agentRunning
	a.client = newAirlockClient(server.URL, "token", server.Client())
	a.RegisterDirectory("sources", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Generated source files"})

	r, err := a.OpenFile(t.Context(), "sources/missing.go")
	if r != nil {
		_ = r.Close()
	}
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrDirectoryNotRegistered) {
		t.Fatalf("OpenFile() error = %v, want only ErrNotFound", err)
	}
}

func TestHostDirectoryErrorMapsToSentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"storage path \"sources/main.go\" does not belong to a registered directory"}`))
	}))
	t.Cleanup(server.Close)
	a := newAgentRegistrationState(Config{Description: "test agent"})
	a.phase = agentRunning
	a.client = newAirlockClient(server.URL, "token", server.Client())
	a.RegisterDirectory("sources", DirectoryOpts{Read: AccessInternal, Write: AccessInternal, List: AccessInternal, Description: "Generated source files"})

	_, err := a.StatFileRef(t.Context(), "sources/main.go")
	if !errors.Is(err, ErrDirectoryNotRegistered) {
		t.Fatalf("StatFileRef() error = %v, want ErrDirectoryNotRegistered", err)
	}
}
