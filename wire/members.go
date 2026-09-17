package wire

// MemberUser is directory metadata, not host-attributed execution identity.
type MemberUser struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	PlatformMember bool   `json:"platformMember"`
}

// Member is a current-app identity and highest effective access snapshot across
// actual direct and group-derived grants, including public grants, not a credential.
type Member struct {
	User   MemberUser `json:"user"`
	Access Access     `json:"access"`
}

// ListMembersResponse contains a page of deduplicated current-app members.
// Users with no direct or group-derived grant are excluded; public grants qualify.
// Each member has the highest effective access (admin > user > public).
type ListMembersResponse struct {
	Members    []Member `json:"members"`
	NextCursor string   `json:"nextCursor,omitempty"`
}
