package agentsdk

import (
	"context"
	"fmt"
)

// Seal encrypts plaintext via Airlock and returns an opaque sealed string the
// agent persists in its OWN storage (its database, a file, wherever its domain
// model fits — agent-wide, per-user, per-conversation). The agent never holds
// the encryption key: Airlock seals and opens on its behalf and binds the
// ciphertext to this agent, so no other agent can Unseal it even if the sealed
// value leaks.
//
// Use this for secrets the agent generates at runtime and must reuse across
// runs — e.g. a session token minted by an interactive login. The plaintext is
// registered for redaction (heuristic-gated, like a Secret env var) so it is
// stripped from LLM input.
func (a *Agent) Seal(ctx context.Context, plaintext string) (string, error) {
	a.maybeAddSensitive(plaintext)
	var resp struct {
		Sealed string `json:"sealed"`
	}
	if err := a.client.doJSON(ctx, "POST", "/api/agent/seal",
		map[string]string{"plaintext": plaintext}, &resp); err != nil {
		return "", fmt.Errorf("agentsdk: seal: %w", err)
	}
	return resp.Sealed, nil
}

// Unseal reverses Seal, returning the original plaintext. It fails if the
// sealed value was produced for a different agent or is corrupt. The recovered
// plaintext is registered for redaction.
func (a *Agent) Unseal(ctx context.Context, sealed string) (string, error) {
	var resp struct {
		Plaintext string `json:"plaintext"`
	}
	if err := a.client.doJSON(ctx, "POST", "/api/agent/unseal",
		map[string]string{"sealed": sealed}, &resp); err != nil {
		return "", fmt.Errorf("agentsdk: unseal: %w", err)
	}
	a.maybeAddSensitive(resp.Plaintext)
	return resp.Plaintext, nil
}
