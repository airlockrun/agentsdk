package agentsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	a.RegisterTool(addTool("lookup", "Look up a record"), AccessUser)
	a.RegisterWebhook(&Webhook{
		Path:        "github",
		Handler:     func(ctx context.Context, data []byte, ew *EventWriter) error { return nil },
		Verify:      "hmac",
		Header:      "X-Hub-Signature-256",
		Description: "GitHub events",
	})
	a.RegisterRoute(&Route{
		Method: http.MethodGet, Path: "/status", Access: AccessAdmin, Description: "Agent status",
		Handler: func(http.ResponseWriter, *http.Request) error { return nil },
	})
	a.RegisterTopic(&Topic{Slug: "alerts", Description: "Alerts", Access: AccessUser})
	a.RegisterDirectory("cache", DirectoryOpts{
		Read: AccessAdmin, Write: AccessAdmin, List: AccessAdmin, Description: "Local cache",
	})
	a.RegisterExecEndpoint(&ExecEndpoint{Slug: "runner", Description: "Build runner", Access: AccessAdmin})
	a.RegisterMCP(&MCP{
		Slug: "docs", Name: "Docs", URL: "https://example.com/mcp", AuthMode: MCPAuthNone, Access: AccessUser,
	})
	a.AddInstruction(&Instruction{Text: "Prefer concise answers.", Access: []Access{AccessUser}})
	a.RegisterModel(&ModelSlot{Slug: "writer", Capability: CapText, Description: "Writing model"})
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
	a.RegisterEnvVar(&EnvVar{Slug: "api_key", Description: "API key", Secret: true})
	a.OnStart("hydrate-cache", func(context.Context) error { return nil })
	manifest := a.Manifest()

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
	expected, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(syncReqs[0].Body, expected) {
		t.Fatalf("runtime sync differs from Manifest():\n sync: %s\nmanifest: %s", syncReqs[0].Body, expected)
	}
	if len(body.EnvVars) != 1 || body.EnvVars[0].Slug != "api_key" {
		t.Fatalf("env vars = %+v", body.EnvVars)
	}
	wantAssetHash := fmt.Sprintf("%x", sha256.Sum256([]byte("body{}")))
	if len(body.StaticAssets) != 1 || body.StaticAssets[0].Name != "app.01234567.css" || body.StaticAssets[0].Size != 6 || body.StaticAssets[0].SHA256 != wantAssetHash {
		t.Fatalf("static assets = %+v", body.StaticAssets)
	}
	if len(body.StartupHooks) != 1 || body.StartupHooks[0].Name != "hydrate-cache" {
		t.Fatalf("startup hooks = %+v", body.StartupHooks)
	}
	if len(body.Connections) != 1 || body.Connections[0].Slug != "gmail" {
		t.Fatalf("expected gmail connection in sync batch, got %+v", body.Connections)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "lookup" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if len(body.Routes) != 2 || body.Routes[0].Path != "/static/{name}" || body.Routes[0].Method != http.MethodGet || body.Routes[0].Access != wire.AccessPublic || body.Routes[1].Path != "/status" {
		t.Fatalf("routes = %+v", body.Routes)
	}
	if len(body.Topics) != 1 || body.Topics[0].Slug != "alerts" {
		t.Fatalf("topics = %+v", body.Topics)
	}
	foundCache := false
	for _, directory := range body.Directories {
		foundCache = foundCache || directory.Path == "cache"
	}
	if !foundCache {
		t.Fatalf("directories = %+v", body.Directories)
	}
	if len(body.ExecEndpoints) != 1 || body.ExecEndpoints[0].Slug != "runner" {
		t.Fatalf("exec endpoints = %+v", body.ExecEndpoints)
	}
	if len(body.MCPServers) != 1 || body.MCPServers[0].Slug != "docs" {
		t.Fatalf("MCP servers = %+v", body.MCPServers)
	}
	if len(body.Instructions) != 1 || body.Instructions[0].Text != "Prefer concise answers." {
		t.Fatalf("instructions = %+v", body.Instructions)
	}
	if len(body.ModelSlots) != 1 || body.ModelSlots[0].Slug != "writer" {
		t.Fatalf("model slots = %+v", body.ModelSlots)
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
