package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSyncWithAirlock(t *testing.T) {
	a, mock := testAgent(t)

	a.RegisterConnection(&Connection{
		Slug:     "gmail",
		Name:     "Gmail",
		AuthMode: "oauth2",
	})
	a.RegisterWebhook(&Webhook{
		Path:    "github",
		Handler: func(ctx context.Context, data []byte, ew *EventWriter) error { return nil },
		Verify:  "hmac",
		Header:  "X-Hub-Signature-256",
	})
	a.RegisterCron(&Cron{
		Slug:     "daily",
		Schedule: "0 9 * * *",
		Handler:  func(ctx context.Context, ew *EventWriter) error { return nil },
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
	var body SyncRequest
	if err := json.Unmarshal(syncReqs[0].Body, &body); err != nil {
		t.Fatalf("decode sync body: %v", err)
	}
	if len(body.Connections) != 1 || body.Connections[0].Slug != "gmail" {
		t.Fatalf("expected gmail connection in sync batch, got %+v", body.Connections)
	}
}
