package agentsdk

import (
	"context"

	"github.com/airlockrun/agentsdk/wire"
)

// DirectoryUser contains the public directory fields of a tenant user.
// IDs address users; they do not grant authority or notification eligibility.
type DirectoryUser = wire.DirectoryUser

// ListUsers returns the tenant-wide human directory, including users with public
// app access. It uses the current app credential and works in application-owned
// contexts without borrowing the app owner's identity.
func (a *Agent) ListUsers(ctx context.Context) ([]DirectoryUser, error) {
	if !a.runtimeAvailable() {
		return nil, a.runtimeUnavailable("ListUsers")
	}
	var response wire.ListUsersResponse
	if err := a.client.doJSON(ctx, "GET", "/api/agent/users", nil, &response); err != nil {
		return nil, err
	}
	return response.Users, nil
}
