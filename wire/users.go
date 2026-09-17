package wire

// DirectoryUser is an addressable tenant user, not an authorization credential.
type DirectoryUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type ListUsersResponse struct {
	Users []DirectoryUser `json:"users"`
}
