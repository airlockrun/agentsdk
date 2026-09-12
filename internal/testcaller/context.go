package testcaller

import (
	"context"
	"github.com/airlockrun/agentsdk/wire"
)

type contextKey struct{}

// FromContext returns test-provided caller state, if present.
func FromContext(ctx context.Context) (wire.Caller, bool) {
	if ctx == nil {
		return wire.Caller{}, false
	}
	c, ok := ctx.Value(contextKey{}).(wire.Caller)
	return c, ok
}

// With stores test-provided caller state on ctx.
func With(ctx context.Context, caller wire.Caller) context.Context {
	if err := caller.Validate(); err != nil {
		panic("testcaller: " + err.Error())
	}
	if caller.User != nil {
		user := *caller.User
		caller.User = &user
	}
	if caller.Initiator != nil {
		user := *caller.Initiator
		caller.Initiator = &user
	}
	return context.WithValue(ctx, contextKey{}, caller)
}
