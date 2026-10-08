package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

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
