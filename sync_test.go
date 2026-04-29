package agentsdk

import (
	"context"
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
		Name:     "daily",
		Schedule: "0 9 * * *",
		Handler:  func(ctx context.Context, ew *EventWriter) error { return nil },
	})

	a.syncWithAirlock(context.Background())

	connReqs := mock.RequestsByPath("/api/agent/connections/")
	if len(connReqs) != 1 {
		t.Fatalf("expected 1 connection registration, got %d", len(connReqs))
	}

	syncReqs := mock.RequestsByPath("/api/agent/sync")
	if len(syncReqs) != 1 {
		t.Fatalf("expected 1 sync request, got %d", len(syncReqs))
	}
}
