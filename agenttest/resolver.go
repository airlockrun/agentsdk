package agenttest

import (
	"context"
	"net/http"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/provider"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

// resolveLocalModel is the boundary to Sol's explicit local model resolver.
// Ordinary environments never call it. Provider resolution must not prompt for
// device login or substitute an unrelated provider/auth mode.
func resolveLocalModel(ctx context.Context, model string) (stream.Model, session.ModelLimits, error) {
	return provider.ResolveLocalModel(ctx, provider.LocalModelOptions{
		Model: model, HTTPClient: http.DefaultClient,
		UserAgent: "agentsdk-agenttest/" + agentsdk.Version,
		SessionID: uuid.NewString(),
	})
}
