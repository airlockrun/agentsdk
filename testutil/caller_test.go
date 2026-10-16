package testutil_test

import (
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/testutil"
	"github.com/airlockrun/agentsdk/wire"
)

func TestExplicitCallerSnapshotDoesNotMaterializeRuntime(t *testing.T) {
	for _, caller := range []wire.Caller{
		{Kind: "anonymous", Access: wire.AccessPublic, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}},
		{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: "startup"}},
	} {
		t.Run(caller.Kind, func(t *testing.T) {
			ctx := testutil.WithCaller(t.Context(), caller)
			actual := agentsdk.CallerFromContext(ctx)
			if string(actual.Kind()) != caller.Kind || string(actual.Access()) != string(caller.Access) {
				t.Fatalf("caller mismatch")
			}
			if _, human := actual.User(); human {
				t.Fatal("nonhuman work received a human owner")
			}
		})
	}
}

func TestHumanCallerSnapshotIsExplicitAndCopied(t *testing.T) {
	for _, access := range []wire.Access{wire.AccessPublic, wire.AccessUser, wire.AccessAdmin} {
		t.Run(string(access), func(t *testing.T) {
			user := wire.CallerUser{ID: "11111111-1111-1111-1111-111111111111", PlatformMember: true, DisplayName: "Original"}
			ctx := testutil.WithCaller(t.Context(), wire.Caller{Kind: "user", Access: access, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}})
			user.DisplayName = "Mutated"
			caller := agentsdk.CallerFromContext(ctx)
			actual, ok := caller.User()
			if !ok || actual.DisplayName != "Original" || !actual.PlatformMember || string(caller.Access()) != string(access) {
				t.Fatalf("caller=%+v", actual)
			}
		})
	}
}
func TestCallerFixtureRejectsMissingIdentityOrOrigin(t *testing.T) {
	for _, caller := range []wire.Caller{{}, {Kind: "user", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}, {Kind: "application", Access: wire.AccessAdmin}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("invalid caller accepted")
				}
			}()
			testutil.WithCaller(t.Context(), caller)
		}()
	}
}
