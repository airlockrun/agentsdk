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
	jobHandle := RegisterJob(a, testJobDefinition(1))
	jobHandle.Cron(&JobCron[testJobInput]{
		Slug:        "daily",
		Schedule:    "0 9 * * *",
		Input:       testJobInput{Source: "uploads/daily.mov"},
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
	if len(body.JobHandlers) != 1 {
		t.Fatalf("job handlers = %d, want 1", len(body.JobHandlers))
	}
	job := body.JobHandlers[0]
	if job.Name != "convert_video" || job.Version != 1 || job.MaxAttempts != 3 || job.MaxConcurrency != 2 {
		t.Fatalf("job handler = %+v", job)
	}
	if len(job.InputSchemaHash) != 64 || len(job.OutputSchemaHash) != 64 {
		t.Fatalf("job schema hashes = %q, %q", job.InputSchemaHash, job.OutputSchemaHash)
	}
	if len(body.JobCrons) != 1 {
		t.Fatalf("job crons = %d, want 1", len(body.JobCrons))
	}
	cron := body.JobCrons[0]
	if cron.Slug != "daily" || cron.Schedule != "0 9 * * *" || cron.Description != "Daily task" ||
		cron.HandlerName != job.Name || cron.HandlerVersion != job.Version ||
		cron.InputSchemaHash != job.InputSchemaHash || cron.OutputSchemaHash != job.OutputSchemaHash ||
		string(cron.Input) != `{"source":"uploads/daily.mov"}` {
		t.Fatalf("job cron = %+v", cron)
	}
}
