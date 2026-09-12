package agentsdk

import (
	"context"
	"testing"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

func testWireCaller(kind string, access wire.Access) wire.Caller {
	c := wire.Caller{Kind: kind, Access: access, Origin: wire.CallerOrigin{Interface: "unknown", Execution: "unknown"}}
	if kind == "user" {
		c.User = &wire.CallerUser{ID: "test-user", Email: "user@example.com", DisplayName: "Test User", PlatformMember: true}
		c.Initiator = c.User
	}
	return c
}

func TestCallerFromContext(t *testing.T) {
	for _, kind := range []string{"anonymous", "user", "application"} {
		t.Run(kind, func(t *testing.T) {
			w := testWireCaller(kind, wire.AccessPublic)
			w.Origin = wire.CallerOrigin{Interface: "chat", Platform: "slack", ClientID: "client", Execution: "request"}
			c := callerFromWire(w)
			outer := testcaller.With(context.Background(), testWireCaller("user", wire.AccessAdmin))
			for _, source := range []string{"run", "lazy", "test"} {
				t.Run(source, func(t *testing.T) {
					var ctx context.Context
					var lazy *lazyRun
					switch source {
					case "run":
						r := newRun(nil, "run", "", "", outer)
						r.setCaller(c)
						ctx = contextWithRun(outer, r)
					case "lazy":
						lazy = &lazyRun{caller: c}
						ctx = contextWithLazyRun(outer, lazy)
					case "test":
						ctx = testcaller.With(outer, w)
					}
					got := CallerFromContext(ctx)
					if got != c || got.Kind() != CallerKind(kind) || got.Access() != AccessPublic {
						t.Fatalf("caller = %+v, want %+v", got, c)
					}
					user, hasUser := got.User()
					initiator, hasInitiator := got.Initiator()
					if hasUser != (kind == "user") || hasInitiator != hasUser || user != initiator {
						t.Fatalf("user=%+v/%t initiator=%+v/%t", user, hasUser, initiator, hasInitiator)
					}
					origin := got.Origin()
					if origin != c.origin {
						t.Fatal("lost origin")
					}
					user.ID, initiator.ID, origin.Platform = "changed", "changed", "changed"
					if CallerFromContext(ctx) != c {
						t.Fatal("snapshot mutated context")
					}
					if lazy != nil && lazy.materialized() != nil {
						t.Fatal("lookup materialized a run")
					}
				})
			}
		})
	}
}

func TestCallerInvalidPanics(t *testing.T) {
	for name, call := range map[string]func(){
		"nil context":    func() { CallerFromContext(nil) },
		"absent context": func() { CallerFromContext(context.Background()) },
		"invalid run masks outer caller": func() {
			CallerFromContext(contextWithRun(testcaller.With(context.Background(), testWireCaller("anonymous", wire.AccessPublic)), &run{}))
		},
		"kind":      func() { (Caller{}).Kind() },
		"access":    func() { (Caller{}).Access() },
		"user":      func() { (Caller{}).User() },
		"initiator": func() { (Caller{}).Initiator() },
		"origin":    func() { (Caller{}).Origin() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("missing panic")
				}
			}()
			call()
		})
	}
}
