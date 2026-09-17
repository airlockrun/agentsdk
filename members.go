package agentsdk

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/airlockrun/agentsdk/wire"
)

// Member contains a current-app member's identity and highest effective access
// across actual direct and group-derived grants, including public grants.
// IDs and access snapshots do not grant authority or notification eligibility.
type Member struct {
	User   User
	Access Access
}

// ListMembersOptions selects a page of the current app's member directory.
type ListMembersOptions struct {
	// Limit is 1 through 1000, or zero for the host default of 100.
	Limit  int
	Cursor string
}

// MemberPage contains deduplicated current-app members and an opaque next-page cursor.
// An empty NextCursor indicates the final page.
type MemberPage struct {
	Members    []Member
	NextCursor string
}

// ListMembers returns a page of the current app's members with actual direct or
// group-derived grants, including public grants. Users with no grant are excluded.
// Users are deduplicated with their highest effective access (admin > user > public).
// Each user's PlatformMember is true. The request uses the current app credential
// and works in application-owned contexts without borrowing the app owner's identity.
func (a *Agent) ListMembers(ctx context.Context, opts ListMembersOptions) (MemberPage, error) {
	if !a.runtimeAvailable() {
		return MemberPage{}, a.runtimeUnavailable("ListMembers")
	}
	if opts.Limit < 0 || opts.Limit > 1000 {
		return MemberPage{}, errors.New("agentsdk: member list limit must be between 0 and 1000")
	}
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	path := "/api/agent/members"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var response wire.ListMembersResponse
	if err := a.client.doJSON(ctx, "GET", path, nil, &response); err != nil {
		return MemberPage{}, err
	}
	page := MemberPage{Members: make([]Member, 0, len(response.Members)), NextCursor: response.NextCursor}
	for _, member := range response.Members {
		page.Members = append(page.Members, Member{User: User(member.User), Access: Access(member.Access)})
	}
	return page, nil
}
