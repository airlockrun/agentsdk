// Package testutil supplies deterministic SDK domain-test fixtures. It starts
// no runtime and performs no credential, network, model or database lookup.
package testutil

import (
	"context"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

// WithCaller injects an explicit validated snapshot for a direct domain/handler
// unit test. It does not authenticate an HTTP delivery or authorize remote work.
func WithCaller(ctx context.Context, caller wire.Caller) context.Context {
	return testcaller.With(ctx, caller)
}
