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
	"github.com/airlockrun/agentsdk/wire"
)

func TestCallerContexts(t *testing.T) {
	user := agentsdk.User{
		ID:             "11111111-1111-1111-1111-111111111111",
		Email:          "alice@example.com",
		DisplayName:    "Alice",
		PlatformMember: true,
	}

	t.Run("plain context panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("missing panic")
			}
		}()
		agentsdk.CallerFromContext(context.Background())
	})

	t.Run("user identity", func(t *testing.T) {
		got, ok := agentsdk.CallerFromContext(agenttest.WithUser(context.Background(), user)).User()
		if !ok || got != user {
			t.Fatalf("Caller.User() = %+v, %t; want %+v, true", got, ok, user)
		}
	})

	t.Run("caller access preserves identity", func(t *testing.T) {
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessPublic)
		got, ok := agentsdk.CallerFromContext(ctx).User()
		if !ok || got != user {
			t.Fatalf("Caller.User() = %+v, %t; want %+v, true", got, ok, user)
		}
	})

}

func TestWithCallerInfo(t *testing.T) {
	for _, kind := range []agentsdk.CallerKind{agentsdk.CallerAnonymous, agentsdk.CallerUser, agentsdk.CallerApplication} {
		t.Run(string(kind), func(t *testing.T) {
			info := agenttest.CallerInfo{Kind: kind, Access: agentsdk.AccessPublic,
				Origin: agentsdk.Origin{Interface: agentsdk.InterfaceHTTP, Platform: "test-platform", ClientID: "client", Execution: agentsdk.ExecutionRequest}}
			if kind == agentsdk.CallerUser {
				// Public nonmember metadata does not establish external authentication.
				info.User = &agentsdk.User{ID: "deliberately-not-a-UUID", DisplayName: "Person"}
				info.Initiator = info.User
			}
			ctx := agenttest.WithCallerInfo(t.Context(), info)
			caller := agentsdk.CallerFromContext(ctx)
			if caller.Kind() != kind || caller.Access() != info.Access || caller.Origin() != info.Origin {
				t.Fatal("lost test metadata")
			}
			if user, ok := caller.User(); ok != (kind == agentsdk.CallerUser) || (ok && user != *info.User) {
				t.Fatalf("user=%+v/%t", user, ok)
			}
			if info.User != nil {
				info.User.ID = "mutated"
			}
			info.Origin.Platform = "mutated"
			if agentsdk.CallerFromContext(ctx) != caller {
				t.Fatal("test metadata is not a snapshot")
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			agenttest.SetCallerHeader(r)
			decoded, err := wire.DecodeCallerHeader(r.Header)
			if err != nil || decoded.Kind != string(kind) || decoded.Origin.Platform != caller.Origin().Platform {
				t.Fatalf("header=%+v err=%v", decoded, err)
			}
			if decoded.User != nil && decoded.User.PlatformMember {
				t.Fatal("test helper invented membership")
			}
		})
	}
}

func TestWithUserSetsMembership(t *testing.T) {
	user := agentsdk.User{ID: "malformed-ID"}
	ctx := agenttest.WithUser(t.Context(), user)
	got, ok := agentsdk.CallerFromContext(ctx).User()
	if !ok || !got.PlatformMember || got.ID != user.ID || user.PlatformMember {
		t.Fatal("helper must set membership on its own snapshot")
	}
}

func TestWithCallerInfoRequiresMetadata(t *testing.T) {
	for name, change := range map[string]func(*agenttest.CallerInfo){
		"kind":      func(c *agenttest.CallerInfo) { c.Kind = "" },
		"access":    func(c *agenttest.CallerInfo) { c.Access = "" },
		"interface": func(c *agenttest.CallerInfo) { c.Origin.Interface = "" },
		"execution": func(c *agenttest.CallerInfo) { c.Origin.Execution = "" },
	} {
		t.Run(name, func(t *testing.T) {
			info := agenttest.CallerInfo{Kind: agentsdk.CallerAnonymous, Access: agentsdk.AccessPublic,
				Origin: agentsdk.Origin{Interface: agentsdk.InterfaceUnknown, Execution: agentsdk.ExecutionUnknown}}
			change(&info)
			defer func() {
				if recover() == nil {
					t.Fatal("missing panic")
				}
			}()
			agenttest.WithCallerInfo(t.Context(), info)
		})
	}
}

func TestSetCallerHeaderRequiresExplicitCaller(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("missing panic")
		}
	}()
	agenttest.SetCallerHeader(httptest.NewRequest(http.MethodGet, "/", nil))
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
		ID:             "11111111-1111-1111-1111-111111111111",
		Email:          "alice@example.com",
		DisplayName:    "Alice",
		PlatformMember: true,
	}
	type observation struct {
		user      agentsdk.User
		hasUser   bool
		path      agentsdk.FilePath
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
			got.user, got.hasUser = agentsdk.CallerFromContext(r.Context()).User()
			got.path, got.accessErr = a.ResolveFilePath(r.Context(), "members/file.txt", agentsdk.FileOperationRead)
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
		req.Header.Set("Authorization", "Bearer test-token")
		agenttest.SetCallerHeader(req)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, user, true)
	})

	t.Run("identity and access are independent", func(t *testing.T) {
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessPublic)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer test-token")
		agenttest.SetCallerHeader(req)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, user, false)
	})

	t.Run("flat headers cannot override caller header", func(t *testing.T) {
		headerUser := agentsdk.User{ID: "22222222-2222-2222-2222-222222222222", Email: "bob@example.com", DisplayName: "Bob"}
		ctx := agenttest.WithCaller(context.Background(), user, agentsdk.AccessAdmin)
		req := httptest.NewRequest(http.MethodGet, "/caller", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer test-token")
		agenttest.SetCallerHeader(req)
		req.Header.Set("X-Caller-Access", string(agentsdk.AccessPublic))
		req.Header.Set("X-User-ID", headerUser.ID)
		req.Header.Set("X-User-Email", headerUser.Email)
		req.Header.Set("X-User-Name", headerUser.DisplayName)
		env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
		assertObservation(t, got, user, true)
	})
}

func assertObservation(t *testing.T, got struct {
	user      agentsdk.User
	hasUser   bool
	path      agentsdk.FilePath
	accessErr error
}, wantUser agentsdk.User, wantAccess bool) {
	t.Helper()
	if !got.hasUser || got.user != wantUser {
		t.Errorf("Caller.User() = %+v, %t; want %+v, true", got.user, got.hasUser, wantUser)
	}
	if wantAccess && got.accessErr != nil {
		t.Errorf("ResolveFilePath() error = %v, want nil", got.accessErr)
	}
	if wantAccess && got.path != "members/file.txt" {
		t.Errorf("ResolveFilePath() = %q, want members/file.txt", got.path)
	}
	if !wantAccess && !errors.Is(got.accessErr, agentsdk.ErrNotFound) {
		t.Errorf("ResolveFilePath() error = %v, want ErrNotFound", got.accessErr)
	}
}
