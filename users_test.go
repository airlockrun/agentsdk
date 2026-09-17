package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestListUsers(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/agent/users" || r.Header.Get("Authorization") != "Bearer app-token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(wire.ListUsersResponse{Users: []wire.DirectoryUser{{ID: "user-id", Email: "user@example.com", DisplayName: "User"}}})
			}))
			defer srv.Close()
			a := &Agent{phase: agentRunning, client: newAirlockClient(srv.URL, "app-token", srv.Client())}
			users, err := a.ListUsers(context.Background())
			if status != http.StatusOK {
				if err == nil {
					t.Fatal("host failure was ignored")
				}
				return
			}
			if err != nil || len(users) != 1 || users[0].ID != "user-id" || users[0].DisplayName != "User" {
				t.Fatalf("ListUsers: %+v, %v", users, err)
			}
		})
	}
}
