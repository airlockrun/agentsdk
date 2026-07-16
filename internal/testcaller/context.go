package testcaller

import "context"

type contextKey struct{}

// Caller is test-provided caller state. The SDK treats it as a fallback after
// framework run state and authenticated request headers.
type Caller struct {
	UserID      string
	Email       string
	DisplayName string
	Access      string
}

// FromContext returns test-provided caller state, if present.
func FromContext(ctx context.Context) (Caller, bool) {
	if ctx == nil {
		return Caller{}, false
	}
	c, ok := ctx.Value(contextKey{}).(Caller)
	return c, ok
}

// With stores test-provided caller state on ctx.
func With(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, contextKey{}, caller)
}
