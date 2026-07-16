package agentsdk

import (
	"context"
	"testing"

	"github.com/airlockrun/agentsdk/internal/testcaller"
)

func TestUserFromContext(t *testing.T) {
	a, _ := testAgent(t)

	// No run on the ctx (e.g. plain background) → absent.
	if u, ok := UserFromContext(context.Background()); ok {
		t.Errorf("UserFromContext(background) = %+v, true; want absent", u)
	}

	// A /prompt-style run carries id + display claims.
	r := newRun(a, "run-1", "", "conv-1", context.Background())
	r.userID = "11111111-1111-1111-1111-111111111111"
	r.userEmail = "alice@example.com"
	r.userDisplayName = "Alice"
	u, ok := UserFromContext(contextWithRun(context.Background(), r))
	if !ok {
		t.Fatal("UserFromContext: ok=false, want true")
	}
	if u.ID != r.userID || u.Email != "alice@example.com" || u.DisplayName != "Alice" {
		t.Errorf("UserFromContext = %+v", u)
	}

	// A cron/schedule/webhook run has no user → absent.
	r2 := newRun(a, "run-2", "", "", context.Background())
	if u, ok := UserFromContext(contextWithRun(context.Background(), r2)); ok {
		t.Errorf("UserFromContext(no user) = %+v, true; want absent", u)
	}
}

func TestUserFromContextFrameworkStatePrecedesTestCaller(t *testing.T) {
	a, _ := testAgent(t)
	testCtx := testcaller.With(context.Background(), testcaller.Caller{
		UserID:      "test-user",
		Email:       "test@example.com",
		DisplayName: "Test User",
		Access:      string(AccessUser),
	})

	t.Run("run", func(t *testing.T) {
		r := newRun(a, "run-1", "", "", context.Background())
		r.userID = "real-user"
		got, ok := UserFromContext(contextWithRun(testCtx, r))
		if !ok || got.ID != "real-user" {
			t.Fatalf("UserFromContext() = %+v, %t; want real-user", got, ok)
		}
	})

	t.Run("anonymous run", func(t *testing.T) {
		r := newRun(a, "run-1", "", "", context.Background())
		if got, ok := UserFromContext(contextWithRun(testCtx, r)); ok {
			t.Fatalf("UserFromContext() = %+v, true; want absent", got)
		}
	})

	t.Run("lazy run", func(t *testing.T) {
		lazy := &lazyRun{agent: a, userID: "real-user", callerAccess: AccessAdmin}
		got, ok := UserFromContext(contextWithLazyRun(testCtx, lazy))
		if !ok || got.ID != "real-user" {
			t.Fatalf("UserFromContext() = %+v, %t; want real-user", got, ok)
		}
	})
}
