package agenttest

import (
	"context"
	"net/http"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

// WithUser returns a context for an authenticated user. It implies AccessUser.
func WithUser(ctx context.Context, user agentsdk.User) context.Context {
	return WithCaller(ctx, user, agentsdk.AccessUser)
}

// WithCaller returns a context for an authenticated user with the given access.
// Identity does not determine access, so tests can model any valid combination.
func WithCaller(ctx context.Context, user agentsdk.User, access agentsdk.Access) context.Context {
	user.PlatformMember = true
	return WithCallerInfo(ctx, CallerInfo{
		Kind: agentsdk.CallerUser, Access: access, User: &user, Initiator: &user,
		Origin: agentsdk.Origin{Interface: agentsdk.InterfaceUnknown, Execution: agentsdk.ExecutionUnknown},
	})
}

// CallerInfo explicitly specifies a test execution. Kind, Access, and Origin's
// Interface and Execution are required. User callers require identical User and
// Initiator snapshots. Use the explicit Unknown constants for unspecified origins.
type CallerInfo struct {
	Kind      agentsdk.CallerKind
	Access    agentsdk.Access
	User      *agentsdk.User
	Initiator *agentsdk.User
	Origin    agentsdk.Origin
}

// WithCallerInfo validates and copies metadata without resolving IDs or doing I/O.
func WithCallerInfo(ctx context.Context, info CallerInfo) context.Context {
	c := wire.Caller{Kind: string(info.Kind), Access: wire.Access(info.Access), Origin: wire.CallerOrigin{
		Interface: string(info.Origin.Interface), Platform: info.Origin.Platform,
		ClientID: info.Origin.ClientID, Execution: string(info.Origin.Execution),
	}}
	if info.User != nil {
		user := wire.CallerUser(*info.User)
		c.User = &user
	}
	if info.Initiator != nil {
		user := wire.CallerUser(*info.Initiator)
		c.Initiator = &user
	}
	return testcaller.With(ctx, c)
}

// SetCallerHeader encodes explicit test caller context for an HTTP delivery.
// It does not authenticate the request; tests must also set the app bearer token.
func SetCallerHeader(r *http.Request) {
	c, ok := testcaller.FromContext(r.Context())
	if !ok {
		panic("agenttest: SetCallerHeader requires an explicit test caller")
	}
	value, err := wire.EncodeCallerHeader(c)
	if err != nil {
		panic("agenttest: " + err.Error())
	}
	r.Header.Set(wire.CallerHeader, value)
}
