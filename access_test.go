package agentsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/airlockrun/agentsdk/internal/testcaller"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "canonical", path: "reports/q1.csv", want: "reports/q1.csv"},
		{name: "trailing slash", path: "reports/q1.csv/", want: "reports/q1.csv"},
		{name: "bare directory", path: "reports", want: "reports"},
		{name: "empty"},
		{name: "root", path: "/"},
		{name: "leading slash", path: "/reports/q1.csv"},
		{name: "traversal", path: "reports/../etc/passwd"},
		{name: "empty segment", path: "reports//q1.csv"},
		{name: "dot segment", path: "reports/."},
		{name: "nul", path: "reports/a\x00b"},
		{name: "backslash", path: `reports\file`},
		{name: "control", path: "reports/a\nb"},
		{name: "invalid UTF-8", path: string([]byte{'a', '/', 0xff})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePath(tt.path)
			if tt.want == "" {
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("normalizePath(%q) error = %v, want ErrInvalidPath", tt.path, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("normalizePath(%q) = %q, %v; want %q", tt.path, got, err, tt.want)
			}
		})
	}
}

func TestResolveFilePathBasicPolicy(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("reports", DirectoryOpts{Read: AccessUser, Write: AccessUser, List: AccessUser, Description: "Reports"})
	a.RegisterDirectory("reports/public", DirectoryOpts{Read: AccessPublic, Write: AccessAdmin, List: AccessPublic, Description: "Public reports"})

	public := withCaller(context.Background(), caller{Access: AccessPublic})
	user := withCaller(context.Background(), caller{Access: AccessUser})
	admin := withCaller(context.Background(), caller{Access: AccessAdmin})

	tests := []struct {
		name string
		ctx  context.Context
		path string
		op   FileOperation
		want string
		err  error
	}{
		{name: "longest prefix", ctx: public, path: "reports/public/q1.csv", op: FileOperationRead, want: "reports/public/q1.csv"},
		{name: "outer directory denied", ctx: public, path: "reports/private/q1.csv", op: FileOperationRead, err: ErrNotFound},
		{name: "user read", ctx: user, path: "reports/q1.csv", op: FileOperationRead, want: "reports/q1.csv"},
		{name: "delete uses write cap", ctx: user, path: "reports/q1.csv", op: FileOperationDelete, want: "reports/q1.csv"},
		{name: "independent list cap", ctx: public, path: "reports", op: FileOperationList, err: ErrNotFound},
		{name: "admin registered path", ctx: admin, path: "reports/private/q1.csv", op: FileOperationRead, want: "reports/private/q1.csv"},
		{name: "uncovered", ctx: admin, path: "nowhere/file", op: FileOperationRead, err: ErrNotFound},
		{name: "malformed", ctx: admin, path: "../secret", op: FileOperationRead, err: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.ResolveFilePath(tt.ctx, tt.path, tt.op)
			if !errors.Is(err, tt.err) {
				t.Fatalf("ResolveFilePath() error = %v, want %v", err, tt.err)
			}
			if string(got) != tt.want {
				t.Fatalf("ResolveFilePath() = %q, want %q", got, tt.want)
			}
		})
	}
	if _, err := a.ResolveFilePath(admin, "reports/q1.csv", FileOperation("future")); err == nil {
		t.Fatal("unknown operation accepted")
	}
}

func TestResolveFilePathExactScopes(t *testing.T) {
	a, _ := testAgent(t)
	for path, scope := range map[string]DirectoryScope{
		"users":         ScopeUser,
		"conversations": ScopeConversation,
		"runs":          ScopeRun,
	} {
		a.RegisterDirectory(path, DirectoryOpts{
			Read: AccessPublic, Write: AccessPublic, List: AccessPublic,
			Scope: scope, Description: path,
		})
	}
	r := newRun(a, "run-current", "", "conv-current", context.Background())
	r.callerAccess = AccessPublic
	r.userID = "user-current"
	ctx := r.checkedCtx()

	tests := []struct {
		name string
		path string
		op   FileOperation
		want string
		err  error
	}{
		{name: "user write insertion", path: "users/file.txt", op: FileOperationWrite, want: "users/user-user-current/file.txt"},
		{name: "user list root insertion", path: "users", op: FileOperationList, want: "users/user-user-current"},
		{name: "conversation nested list insertion", path: "conversations/archive", op: FileOperationList, want: "conversations/conv-conv-current/archive"},
		{name: "run write insertion", path: "runs/file.txt", op: FileOperationWrite, want: "runs/run-run-current/file.txt"},
		{name: "matching read", path: "users/user-user-current/file.txt", op: FileOperationRead, want: "users/user-user-current/file.txt"},
		{name: "matching overwrite", path: "users/user-user-current/file.txt", op: FileOperationOverwrite, want: "users/user-user-current/file.txt"},
		{name: "matching delete", path: "users/user-user-current/file.txt", op: FileOperationDelete, want: "users/user-user-current/file.txt"},
		{name: "bare read denied", path: "users/file.txt", op: FileOperationRead, err: ErrNotFound},
		{name: "bare overwrite denied", path: "users/file.txt", op: FileOperationOverwrite, err: ErrNotFound},
		{name: "bare delete denied", path: "users/file.txt", op: FileOperationDelete, err: ErrNotFound},
		{name: "cross user denied", path: "users/user-other/file.txt", op: FileOperationRead, err: ErrNotFound},
		{name: "wrong scope kind denied", path: "users/conv-conv-current/file.txt", op: FileOperationWrite, err: ErrNotFound},
		{name: "hyphenated ordinary path", path: "users/draft-2026/file.txt", op: FileOperationWrite, want: "users/user-user-current/draft-2026/file.txt"},
		{name: "write root invalid", path: "users", op: FileOperationWrite, err: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.ResolveFilePath(ctx, tt.path, tt.op)
			if !errors.Is(err, tt.err) {
				t.Fatalf("ResolveFilePath() error = %v, want %v", err, tt.err)
			}
			if string(got) != tt.want {
				t.Fatalf("ResolveFilePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveFilePathDoesNotFallbackIdentity(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("private", DirectoryOpts{
		Read: AccessPublic, Write: AccessPublic, List: AccessPublic,
		Scope: ScopeUser, Description: "Private",
	})
	r := newRun(a, "run-current", "", "conv-current", context.Background())
	r.callerAccess = AccessPublic
	if _, err := a.ResolveFilePath(r.checkedCtx(), "private/file.txt", FileOperationWrite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user identity error = %v, want ErrNotFound", err)
	}
}

func TestResolveFilePathTestCallerUser(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("private", DirectoryOpts{
		Read: AccessPublic, Write: AccessPublic, List: AccessPublic,
		Scope: ScopeUser, Description: "Private",
	})
	ctx := testcaller.With(context.Background(), testcaller.Caller{UserID: "test-user", Access: string(AccessPublic)})
	got, err := a.ResolveFilePath(ctx, "private/user-test-user/file.txt", FileOperationRead)
	if err != nil || got != "private/user-test-user/file.txt" {
		t.Fatalf("ResolveFilePath() = %q, %v", got, err)
	}
}

func TestCallerFromContextFrameworkStatePrecedesTestCaller(t *testing.T) {
	a, _ := testAgent(t)
	testCtx := testcaller.With(context.Background(), testcaller.Caller{Access: string(AccessPublic)})
	r := newRun(a, "run-1", "", "", context.Background())
	r.callerAccess = AccessAdmin
	if got := callerFromContext(contextWithRun(testCtx, r)).Access; got != AccessAdmin {
		t.Errorf("run caller access = %q, want %q", got, AccessAdmin)
	}
	lazy := &lazyRun{agent: a, callerAccess: AccessUser}
	if got := callerFromContext(contextWithLazyRun(testCtx, lazy)).Access; got != AccessUser {
		t.Errorf("lazy-run caller access = %q, want %q", got, AccessUser)
	}
}
