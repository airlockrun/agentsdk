package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/session"
)

func TestHTTPSessionStoreTracksRevision(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			if r.Method != http.MethodGet {
				t.Errorf("load method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(wire.SessionLoadResponse{
				Messages: []session.Message{{Role: "user", Content: "hello"}},
				Revision: "rev-1",
			})
		case 2:
			var req wire.SessionAppendRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode append: %v", err)
			}
			if req.Revision != "rev-1" {
				t.Errorf("append revision = %q, want rev-1", req.Revision)
			}
			if r.URL.Query().Get("runId") != "run-1" || r.URL.Query().Get("source") != "user" {
				t.Errorf("append query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(wire.SessionAppendResponse{Revision: "rev-2"})
		case 3:
			var req wire.SessionCompactRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode compact: %v", err)
			}
			if req.Revision != "rev-2" {
				t.Errorf("compact revision = %q, want rev-2", req.Revision)
			}
			_ = json.NewEncoder(w).Encode(wire.SessionCompactResponse{Revision: "rev-3"})
		default:
			t.Errorf("unexpected request %d", calls)
		}
	}))
	defer server.Close()

	store := newHTTPSessionStore(newAirlockClient(server.URL, "token", server.Client()), "conv-1", "run-1", "user")
	msgs, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Fatalf("loaded messages = %#v", msgs)
	}
	if err := store.Append(context.Background(), []session.Message{{Role: "assistant", Content: "world"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Compact(context.Background(), []session.Message{{Role: "assistant", Content: "summary"}}, 10); err != nil {
		t.Fatalf("Compact: %v", err)
	}
}

func TestHTTPSessionStoreRequiresRevision(t *testing.T) {
	store := newHTTPSessionStore(newAirlockClient("http://unused", "token", http.DefaultClient), "conv-1", "run-1", "")
	if err := store.Append(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "prior load") {
		t.Fatalf("Append error = %v, want prior-load error", err)
	}
	if err := store.Compact(context.Background(), []session.Message{{Role: "assistant", Content: "summary"}}, 0); err == nil || !strings.Contains(err.Error(), "prior load") {
		t.Fatalf("Compact error = %v, want prior-load error", err)
	}
}
