package agentsdk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestListMembers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		opts  ListMembersOptions
		query string
	}{
		{name: "default"},
		{name: "minimum", opts: ListMembersOptions{Limit: 1}, query: "limit=1"},
		{name: "maximum", opts: ListMembersOptions{Limit: 1000}, query: "limit=1000"},
		{name: "cursor", opts: ListMembersOptions{Cursor: "a+b/=& ?"}, query: "cursor=a%2Bb%2F%3D%26+%3F"},
		{name: "page", opts: ListMembersOptions{Limit: 2, Cursor: "next"}, query: "cursor=next&limit=2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/agent/members" || r.URL.RawQuery != tt.query || r.Header.Get("Authorization") != "Bearer app-token" {
					t.Errorf("unexpected request: %s %s, authorization %q", r.Method, r.URL, r.Header.Get("Authorization"))
				}
				_, _ = io.WriteString(w, `{"members":[{"user":{"id":"user-id","email":"user@example.com","displayName":"User","platformMember":true},"access":"public"},{"user":{"id":"admin-id","email":"admin@example.com","displayName":"Admin","platformMember":true},"access":"admin"}],"nextCursor":"next+cursor"}`)
			}))
			defer srv.Close()
			a := &Agent{phase: agentRunning, client: newAirlockClient(srv.URL, "app-token", srv.Client())}
			page, err := a.ListMembers(context.Background(), tt.opts)
			want := MemberPage{Members: []Member{
				{User: User{ID: "user-id", Email: "user@example.com", DisplayName: "User", PlatformMember: true}, Access: AccessPublic},
				{User: User{ID: "admin-id", Email: "admin@example.com", DisplayName: "Admin", PlatformMember: true}, Access: AccessAdmin},
			}, NextCursor: "next+cursor"}
			if err != nil || !reflect.DeepEqual(page, want) {
				t.Fatalf("ListMembers = %+v, %v; want %+v", page, err, want)
			}
		})
	}
}

func TestListMembersErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		limit  int
		status int
		body   string
		want   string
	}{
		{name: "negative limit", limit: -1, want: "limit"},
		{name: "excessive limit", limit: 1001, want: "limit"},
		{name: "unauthorized", status: http.StatusUnauthorized, want: "401"},
		{name: "host failure", status: http.StatusInternalServerError, want: "500"},
		{name: "malformed JSON", status: http.StatusOK, body: `{`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.status == 0 {
					t.Error("invalid limit reached transport")
					return
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			a := &Agent{phase: agentRunning, client: newAirlockClient(srv.URL, "app-token", srv.Client())}
			page, err := a.ListMembers(context.Background(), ListMembersOptions{Limit: tt.limit})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !reflect.DeepEqual(page, MemberPage{}) {
				t.Fatalf("ListMembers = %+v, %v; want zero page and error containing %q", page, err, tt.want)
			}
		})
	}
	t.Run("runtime unavailable", func(t *testing.T) {
		_, err := (&Agent{}).ListMembers(context.Background(), ListMembersOptions{})
		if err == nil || !strings.Contains(err.Error(), "ListMembers") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("canceled request", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("canceled request reached host")
		}))
		defer srv.Close()
		a := &Agent{phase: agentRunning, client: newAirlockClient(srv.URL, "app-token", srv.Client())}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		page, err := a.ListMembers(ctx, ListMembersOptions{})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, MemberPage{}) {
			t.Fatalf("ListMembers = %+v, %v; want zero page and context.Canceled", page, err)
		}
	})
}

func TestListMembersEmptyPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"members":[]}`)
	}))
	defer srv.Close()
	a := &Agent{phase: agentRunning, client: newAirlockClient(srv.URL, "app-token", srv.Client())}
	page, err := a.ListMembers(context.Background(), ListMembersOptions{})
	if err != nil || page.Members == nil || len(page.Members) != 0 || page.NextCursor != "" {
		t.Fatalf("ListMembers = %+v, %v; want empty final page", page, err)
	}
}
