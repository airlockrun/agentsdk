package agentsdk

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestSyncWithAirlock(t *testing.T) {
	a, mock := testAgent(t)

	a.RegisterConnection(&Connection{
		Slug:        "gmail",
		Name:        "Gmail",
		Description: "Gmail API",
		BaseURL:     "https://gmail.googleapis.com",
		AuthMode:    ConnectionAuthOAuth,
		AuthURL:     "https://accounts.google.com/o/oauth2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		Access:      AccessUser,
	})
	a.RegisterWebhook(&Webhook{
		Path:        "github",
		Handler:     func(ctx context.Context, data []byte, ew *EventWriter) error { return nil },
		Verify:      "hmac",
		Header:      "X-Hub-Signature-256",
		Description: "GitHub events",
	})
	a.RegisterCron(&Cron{
		Slug:        "daily",
		Schedule:    "0 9 * * *",
		Handler:     func(ctx context.Context, event ScheduleEvent) error { return nil },
		Description: "Daily task",
	})
	a.RegisterStaticAsset(&StaticAsset{
		Name:        "app.01234567.css",
		ContentType: "text/css",
		Data:        []byte("body{}"),
	})

	a.syncWithAirlock(context.Background())

	// Connections ride the sync batch now, not a per-slug PUT.
	if connReqs := mock.RequestsByPath("/api/agent/connections/"); len(connReqs) != 0 {
		t.Fatalf("expected 0 connection PUTs, got %d", len(connReqs))
	}

	syncReqs := mock.RequestsByPath("/api/agent/sync")
	if len(syncReqs) != 1 {
		t.Fatalf("expected 1 sync request, got %d", len(syncReqs))
	}
	var body wire.SyncRequest
	if err := json.Unmarshal(syncReqs[0].Body, &body); err != nil {
		t.Fatalf("decode sync body: %v", err)
	}
	if len(body.Connections) != 1 || body.Connections[0].Slug != "gmail" {
		t.Fatalf("expected gmail connection in sync batch, got %+v", body.Connections)
	}
	if len(body.Routes) != 1 || body.Routes[0].Path != "/static/{name}" || body.Routes[0].Method != http.MethodGet || body.Routes[0].Access != wire.AccessPublic {
		t.Fatalf("expected public static route in sync batch, got %+v", body.Routes)
	}
}
