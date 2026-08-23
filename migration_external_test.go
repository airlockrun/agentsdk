package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMigrationExternalStepSkipsValidation(t *testing.T) {
	t.Setenv("AGENT_VALIDATE_MIGRATIONS", "1")
	called := false
	if err := MigrationExternalStep(context.Background(), func(context.Context, *Agent) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("external step ran during validation")
	}
}

func TestMigrationExternalStepProvidesAgent(t *testing.T) {
	a := &Agent{}
	ctx := a.migrationContext(context.Background())
	if err := MigrationExternalStep(ctx, func(_ context.Context, got *Agent) error {
		if got != a {
			t.Fatalf("Agent = %p, want %p", got, a)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationExternalStepPanicsOutsideMigration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MigrationExternalStep did not panic")
		}
	}()
	_ = MigrationExternalStep(context.Background(), func(context.Context, *Agent) error { return nil })
}

func TestMigrationExternalStepPanicsForNilCallback(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MigrationExternalStep did not panic")
		}
	}()
	_ = MigrationExternalStep(context.Background(), nil)
}

func TestMoveFileRestartStates(t *testing.T) {
	tests := []struct {
		name       string
		src        bool
		dst        bool
		wantErr    error
		wantCopies int
	}{
		{name: "not started", src: true, wantCopies: 1},
		{name: "copy completed before restart", src: true, dst: true, wantCopies: 1},
		{name: "move completed before restart", dst: true},
		{name: "neither object exists", wantErr: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]bool{"old/file.txt": tt.src, "new/file.txt": tt.dst}
			copies := 0
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/agent/storage/info", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Path string `json:"path"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if !files[body.Path] {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(FileInfo{Path: FilePath(body.Path)})
			})
			mux.HandleFunc("POST /api/agent/storage/copy", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Src string `json:"src"`
					Dst string `json:"dst"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				copies++
				files[body.Dst] = files[body.Src]
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("DELETE /api/agent/storage/{path...}", func(w http.ResponseWriter, r *http.Request) {
				files[strings.TrimPrefix(r.URL.Path, "/api/agent/storage/")] = false
				w.WriteHeader(http.StatusNoContent)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			a := &Agent{httpClient: srv.Client(), phase: agentStarting}
			a.client = newAirlockClient(srv.URL, "test", a.httpClient)

			err := a.MoveFile(context.Background(), "old/file.txt", "new/file.txt")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MoveFile error = %v, want %v", err, tt.wantErr)
			}
			if copies != tt.wantCopies {
				t.Fatalf("copies = %d, want %d", copies, tt.wantCopies)
			}
			if tt.wantErr == nil && (files["old/file.txt"] || !files["new/file.txt"]) {
				t.Fatalf("files after move = %#v", files)
			}
		})
	}
}
