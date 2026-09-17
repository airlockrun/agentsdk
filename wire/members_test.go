package wire

import (
	"encoding/json"
	"testing"
)

func TestListMembersResponseJSON(t *testing.T) {
	for _, cursor := range []string{"", "next"} {
		t.Run("cursor="+cursor, func(t *testing.T) {
			response := ListMembersResponse{Members: []Member{{
				User:   MemberUser{ID: "id", Email: "user@example.com", DisplayName: "User", PlatformMember: true},
				Access: AccessUser,
			}}, NextCursor: cursor}
			data, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"members":[{"user":{"id":"id","email":"user@example.com","displayName":"User","platformMember":true},"access":"user"}]`
			if cursor != "" {
				want += `,"nextCursor":"next"`
			}
			want += `}`
			if string(data) != want {
				t.Fatalf("JSON = %s; want %s", data, want)
			}
		})
	}
}
