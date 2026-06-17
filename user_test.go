package agentsdk

import (
	"context"
	"testing"
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
