package agenttest

import (
	"context"
	"strings"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/internal/testcaller"
)

// WithUser returns a context for an authenticated user. It implies AccessUser.
func WithUser(ctx context.Context, user agentsdk.User) context.Context {
	return WithCaller(ctx, user, agentsdk.AccessUser)
}

// WithCaller returns a context for an authenticated user with the given access.
// Identity does not determine access, so tests can model any valid combination.
func WithCaller(ctx context.Context, user agentsdk.User, access agentsdk.Access) context.Context {
	if strings.TrimSpace(user.ID) == "" {
		panic("agenttest: user ID is required")
	}
	switch access {
	case agentsdk.AccessAdmin, agentsdk.AccessUser, agentsdk.AccessPublic:
	default:
		panic("agenttest: caller access must be AccessAdmin, AccessUser, or AccessPublic")
	}
	return testcaller.With(ctx, testcaller.Caller{
		UserID:      user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Access:      string(access),
	})
}
