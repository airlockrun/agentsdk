package agentsdk

import (
	"context"
	"errors"
	"testing"
)

// TestNormalizePath covers the path-normalization rules — leading /, no
// .., no //, no trailing slash, no root-only.
func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in   string
		want string // empty when an error is expected
	}{
		{"/reports/q1.csv", "/reports/q1.csv"},
		{"/reports/q1.csv/", "/reports/q1.csv"}, // trailing slash stripped
		{"/", ""},                               // root-only rejected
		{"", ""},                                // empty rejected
		{"reports/q1.csv", ""},                  // missing leading /
		{"/reports/../etc/passwd", ""},          // .. segment
		{"/reports//q1.csv", ""},                // empty segment
		{"/.", ""},                              // . segment
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizePath(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCheckFileAccess_UnregisteredPath ensures unknown top segments
// return ErrNotFound (never distinguishing "denied" from "not found").
func TestCheckFileAccess_UnregisteredPath(t *testing.T) {
	a, _ := testAgent(t)
	ctx := WithCaller(context.Background(), Caller{Access: AccessAdmin})
	if err := a.CheckFileAccess(ctx, "/nowhere/foo", OpRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCheckFileAccess_InvalidPath surfaces malformed paths as
// ErrInvalidPath rather than ErrNotFound — different category, useful
// for callers debugging input.
func TestCheckFileAccess_InvalidPath(t *testing.T) {
	a, _ := testAgent(t)
	ctx := WithCaller(context.Background(), Caller{Access: AccessAdmin})
	if err := a.CheckFileAccess(ctx, "../etc/passwd", OpRead); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath, got %v", err)
	}
}

// TestCheckFileAccess_LongestPrefix verifies nested directory
// registrations: the most-specific path covering the request wins.
func TestCheckFileAccess_LongestPrefix(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("/reports", DirectoryOpts{
		Read: AccessUser, Write: AccessAdmin, List: AccessUser,
	})
	a.RegisterDirectory("/reports/public", DirectoryOpts{
		Read: AccessPublic, Write: AccessAdmin, List: AccessPublic,
	})

	publicCtx := WithCaller(context.Background(), Caller{Access: AccessPublic})

	// Path under the more-specific public dir — public read allowed.
	if err := a.CheckFileAccess(publicCtx, "/reports/public/foo.csv", OpRead); err != nil {
		t.Fatalf("expected /reports/public read to succeed for public, got %v", err)
	}
	// Path under the less-specific dir — public read denied.
	if err := a.CheckFileAccess(publicCtx, "/reports/private/foo.csv", OpRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected /reports private read to deny public, got %v", err)
	}
}

// TestCheckFileAccess_FailClosed confirms the default ctx (no Caller
// attached) is treated as AccessPublic and denies anything user-or-above.
func TestCheckFileAccess_FailClosed(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("/reports", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser,
	})
	if err := a.CheckFileAccess(context.Background(), "/reports/x", OpRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected fail-closed deny on plain ctx, got %v", err)
	}
}

// TestCheckFileAccess_DeleteFoldsIntoWrite verifies the POSIX-style
// folding: a caller with Write but not (a separate) delete cap can
// still call deleteFile (because there is no separate cap).
func TestCheckFileAccess_DeleteFoldsIntoWrite(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("/uploads", DirectoryOpts{
		Read: AccessUser, Write: AccessUser, List: AccessUser,
	})
	ctx := WithCaller(context.Background(), Caller{Access: AccessUser})
	// OpWrite is the operation tag for deletes. The agent.DeleteFile
	// path bypasses CheckFileAccess (trusted), but the VM binding for
	// deleteFile() goes through CheckFileAccess(OpWrite) — verify the
	// gate accepts a user-level caller.
	if err := a.CheckFileAccess(ctx, "/uploads/foo", OpWrite); err != nil {
		t.Fatalf("expected user write to succeed, got %v", err)
	}
}

// TestCheckFileAccess_CapsAreIndependent verifies a directory can grant
// list while denying read (browsable but contents-protected) and the
// gate enforces each cap separately.
func TestCheckFileAccess_CapsAreIndependent(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterDirectory("/inbox", DirectoryOpts{
		Read:  AccessAdmin, // contents are admin-only
		Write: AccessUser,  // anyone signed-in can drop a file
		List:  AccessUser,  // anyone can see filenames
	})
	userCtx := WithCaller(context.Background(), Caller{Access: AccessUser})

	if err := a.CheckFileAccess(userCtx, "/inbox/foo", OpRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user should not have read access to admin-read dir, got %v", err)
	}
	if err := a.CheckFileAccess(userCtx, "/inbox/foo", OpList); err != nil {
		t.Fatalf("user should have list access, got %v", err)
	}
	if err := a.CheckFileAccess(userCtx, "/inbox/foo", OpWrite); err != nil {
		t.Fatalf("user should have write access, got %v", err)
	}
}
