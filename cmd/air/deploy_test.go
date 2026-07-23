package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk"
)

func TestCmdDeployExplainsUnavailableBoundAgent(t *testing.T) {
	const (
		boundID = "11111111-1111-1111-1111-111111111111"
		otherID = "22222222-2222-2222-2222-222222222222"
	)
	tests := []struct {
		name       string
		detailCode int
		listBody   string
		want       []string
		wantNoList bool
	}{
		{
			name:       "deleted",
			detailCode: http.StatusNotFound,
			listBody:   `{"agents":[]}`,
			want:       []string{"no longer exists", "No different accessible agent", "go tool air remote unbind prod", "workspace was not rebound", boundID},
		},
		{
			name:       "deleted and slug reused",
			detailCode: http.StatusNotFound,
			listBody:   `{"agents":[{"id":"` + otherID + `","slug":"todo"}]}`,
			want:       []string{"no longer exists", "A different accessible agent now uses slug", "will not deploy to the different agent automatically", boundID, otherID, "go tool air remote unbind prod", "--slug <new-slug>"},
		},
		{
			name:       "access removed",
			detailCode: http.StatusForbidden,
			want:       []string{"still exists", "current login cannot access it", "restore access", "workspace was not rebound", boundID},
			wantNoList: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var listCalls, uploadCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agent-sdk":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"version":%q}`, agentsdk.Version)
				case "/api/v1/agents/" + boundID:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.detailCode)
					if tt.detailCode == http.StatusNotFound {
						_, _ = w.Write([]byte(`{"error":"agent not found"}`))
					} else {
						_, _ = w.Write([]byte(`{"error":"access denied"}`))
					}
				case "/api/v1/agents":
					listCalls++
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tt.listBody))
				case "/api/v1/agents/" + boundID + "/source":
					uploadCalls++
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			if err := saveLoginCredentials(srv.URL, "dev@example.com", "token", ""); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "go.mod"), "module agent\n")
			binding := agentBinding{}
			binding.putRemote("prod", agentRemoteBinding{AirlockURL: srv.URL, AgentID: boundID, Slug: "todo", SourceState: "sha256:old"})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}

			err := cmdDeploy([]string{dir, "--remote", "prod", "-m", "Deploy changes"})
			if err == nil {
				t.Fatal("cmdDeploy returned nil error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			if tt.name == "deleted and slug reused" && strings.Contains(err.Error(), "--create --slug todo") {
				t.Errorf("error recommends recreating occupied slug: %q", err)
			}
			if tt.wantNoList && listCalls != 0 {
				t.Errorf("list calls = %d, want 0", listCalls)
			}
			if !tt.wantNoList && listCalls != 1 {
				t.Errorf("list calls = %d, want 1", listCalls)
			}
			if uploadCalls != 0 {
				t.Errorf("upload calls = %d, want 0", uploadCalls)
			}
		})
	}
}

func TestExplainDeployTargetMismatchRequiresExplicitUnbind(t *testing.T) {
	const (
		boundID = "11111111-1111-1111-1111-111111111111"
		otherID = "22222222-2222-2222-2222-222222222222"
	)
	err := explainDeployTargetError(context.Background(), "https://airlock.example.com", "token", "prod", ".", "todo", agentRemoteBinding{
		AgentID: boundID,
		Slug:    "todo",
	}, &agentBindingMismatchError{
		remoteName:   "prod",
		boundID:      boundID,
		boundSlug:    "todo",
		resolvedID:   otherID,
		resolvedSlug: "todo",
	})
	for _, want := range []string{"workspace was not rebound", "go tool air remote unbind prod", boundID, otherID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestExplainCreateAgentSlugConflict(t *testing.T) {
	const otherID = "22222222-2222-2222-2222-222222222222"
	tests := []struct {
		name     string
		listBody string
		want     []string
	}{
		{
			name:     "accessible agent",
			listBody: `{"agents":[{"id":"` + otherID + `","slug":"todo"}]}`,
			want:     []string{"already has an accessible agent", otherID, "go tool air deploy --remote prod --url", "--agent " + otherID},
		},
		{
			name:     "inaccessible agent",
			listBody: `{"agents":[]}`,
			want:     []string{"already in use", "no accessible agent", "Choose a different --slug"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.listBody))
			}))
			defer srv.Close()
			err := explainCreateAgentError(context.Background(), srv.URL, "token", "prod", ".", "todo", &httpStatusError{
				StatusCode: http.StatusConflict,
				Status:     "409 Conflict",
				Message:    "agent slug already exists",
			})
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestMissingBoundAgentErrorDescribesPersistedCreateBinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	}))
	defer srv.Close()
	binding := agentRemoteBinding{
		AgentID: "11111111-1111-1111-1111-111111111111",
		Slug:    "todo",
	}
	err := missingBoundAgentError(context.Background(), srv.URL, "token", "prod", ".", binding, true)
	if !strings.Contains(err.Error(), "workspace remains locally bound to the missing agent") {
		t.Fatalf("error = %q", err)
	}
	if strings.Contains(err.Error(), "workspace was not rebound") {
		t.Fatalf("error incorrectly describes new binding: %q", err)
	}
}

func TestUploadSourcePreservesDeployStatus(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module agent\n")
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"deployment refused"}`))
			}))
			defer srv.Close()
			_, err := uploadSource(context.Background(), srv.URL, "token", "agent", dir, "", "Deploy", false)
			if !hasHTTPStatus(err, code) {
				t.Fatalf("uploadSource error = %v, want HTTP %d", err, code)
			}
		})
	}
}

func TestUploadSourceDistinguishesPreconditionStatus(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module agent\n")
	for _, code := range []int{http.StatusPreconditionFailed, http.StatusPreconditionRequired} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := uploadSource(context.Background(), srv.URL, "token", "agent", dir, "", "Deploy", false)
			stale, ok := err.(*staleSourceError)
			if !ok {
				t.Fatalf("uploadSource error = %T %v", err, err)
			}
			if stale.statusCode != code {
				t.Fatalf("statusCode = %d, want %d", stale.statusCode, code)
			}
		})
	}
}

func TestDeploySourceStateErrorExplainsPrecondition(t *testing.T) {
	target := agentRemoteBinding{AgentID: "11111111-1111-1111-1111-111111111111", Slug: "todo"}
	tests := []struct {
		name string
		code int
		want string
	}{
		{name: "stale state", code: http.StatusPreconditionFailed, want: "source changed since this workspace last synced"},
		{name: "missing state", code: http.StatusPreconditionRequired, want: "workspace has no synchronized source state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := deploySourceStateError(&staleSourceError{statusCode: tt.code}, target, "https://airlock.example.com", "prod")
			for _, want := range []string{tt.want, "airlock clone", "go tool air deploy", target.AgentID} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}
