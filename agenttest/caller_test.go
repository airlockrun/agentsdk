package agenttest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/agenttest"
)

func TestCallerContexts(t *testing.T) {
	user := agentsdk.User{
		ID:          "11111111-1111-1111-1111-111111111111",
		Email:       "alice@example.com",
		DisplayName: "Alice",
	}

	t.Run("plain context is anonymous", func(t *testing.T) {
		if got, ok := agentsdk.UserFromContext(context.Background()); ok {
			t.Fatalf("UserFromContext() = %+v, true; want absent", got)
		}
	})

	t.Run("user identity", func(t *testing.T) {
		got, ok := agentsdk.UserFromContext(agenttest.WithUser(context.Background(), user))
		if !ok || got != user {
			t.Fatalf("UserFromContext() = %+v, %t; want %+v, true", got, ok, user)
		}
	})

	t.Run("caller access preserves identity", func(t *testing.T) {
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessPublic)
		got, ok := agentsdk.UserFromContext(ctx)
		if !ok || got != user {
			t.Fatalf("UserFromContext() = %+v, %t; want %+v, true", got, ok, user)
		}
	})

}

func TestCallerContextValidation(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{
			name: "empty user ID",
			call: func() {
				agenttest.WithUser(context.Background(), agentsdk.User{})
			},
		},
		{
			name: "whitespace user ID",
			call: func() {
				agenttest.WithUser(context.Background(), agentsdk.User{ID: "  "})
			},
		},
		{
			name: "unknown access",
			call: func() {
				agenttest.WithCaller(context.Background(), agentsdk.User{ID: "user-id"}, agentsdk.Access("owner"))
			},
		},
		{
			name: "caller empty user ID",
			call: func() {
				agenttest.WithCaller(context.Background(), agentsdk.User{}, agentsdk.AccessAdmin)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("call did not panic")
				}
			}()
			tt.call()
		})
	}
}

func TestCallerContextsInDirectAndHTTPHandlers(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module callercontexttest\n\ngo 1.26.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)

	user := agentsdk.User{
		ID:          "11111111-1111-1111-1111-111111111111",
		Email:       "alice@example.com",
		DisplayName: "Alice",
	}
	type observation struct {
		user      agentsdk.User
		hasUser   bool
		accessErr error
	}
	var got observation
	var handler agentsdk.RouteHandlerFunc

	env := agenttest.New(t, func() *agentsdk.Agent {
		a := agentsdk.New(agentsdk.Config{Description: "caller context test"})
		a.RegisterDirectory("members", agentsdk.DirectoryOpts{
			Read:        agentsdk.AccessUser,
			Write:       agentsdk.AccessUser,
			List:        agentsdk.AccessUser,
			Description: "Members",
		})
		handler = func(_ http.ResponseWriter, r *http.Request) error {
			got.user, got.hasUser = agentsdk.UserFromContext(r.Context())
			got.accessErr = a.CheckFileAccess(r.Context(), "members/file.txt", agentsdk.OpRead)
			return nil
		}
		a.RegisterRoute(&agentsdk.Route{
			Method:      http.MethodGet,
			Path:        "/caller",
			Handler:     handler,
			Access:      agentsdk.AccessPublic,
			Description: "Observe the caller context",
		})
		return a
	})

	t.Run("direct handler", func(t *testing.T) {
		ctx := agenttest.WithUser(context.Background(), user)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		if err := handler(httptest.NewRecorder(), req); err != nil {
			t.Fatal(err)
		}
		assertObservation(t, got, user, true)
	})

	t.Run("HTTP handler", func(t *testing.T) {
		ctx := agenttest.WithUser(context.Background(), user)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, user, true)
	})

	t.Run("identity and access are independent", func(t *testing.T) {
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessPublic)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, user, false)
	})

	t.Run("request headers take precedence", func(t *testing.T) {
		headerUser := agentsdk.User{ID: "22222222-2222-2222-2222-222222222222", Email: "bob@example.com", DisplayName: "Bob"}
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessAdmin)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		req.Header.Set("X-Caller-Access", string(agentsdk.AccessPublic))
		req.Header.Set("X-User-ID", headerUser.ID)
		req.Header.Set("X-User-Email", headerUser.Email)
		req.Header.Set("X-User-Name", headerUser.DisplayName)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, headerUser, false)
	})
}

func assertObservation(t *testing.T, got struct {
	user      agentsdk.User
	hasUser   bool
	accessErr error
}, wantUser agentsdk.User, wantAccess bool) {
	t.Helper()
	if !got.hasUser || got.user != wantUser {
		t.Errorf("UserFromContext() = %+v, %t; want %+v, true", got.user, got.hasUser, wantUser)
	}
	if wantAccess && got.accessErr != nil {
		t.Errorf("CheckFileAccess() error = %v, want nil", got.accessErr)
	}
	if !wantAccess && !errors.Is(got.accessErr, agentsdk.ErrNotFound) {
		t.Errorf("CheckFileAccess() error = %v, want ErrNotFound", got.accessErr)
	}
}
