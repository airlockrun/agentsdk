package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListMembersReturnsOwnRoster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/agent/members" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"members": []AgentMember{
			{ID: "00000000-0000-0000-0000-000000000001", Kind: "user", Role: AccessAdmin},
			{ID: "00000000-0000-0000-0000-000000000002", Kind: "group", Role: AccessUser},
		}})
	}))
	t.Cleanup(server.Close)

	agent := New(Config{Description: "member roster test"})
	agent.phase = agentRunning
	agent.client = newAirlockClient(server.URL, "token", server.Client())
	members, err := agent.ListMembers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0].Kind != "user" || members[0].Role != AccessAdmin || members[1].ID != "00000000-0000-0000-0000-000000000002" || members[1].Kind != "group" {
		t.Fatalf("members = %#v", members)
	}
}
