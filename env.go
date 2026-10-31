package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/airlockrun/agentsdk/wire"
)

// EnvVar declares an operator-configured environment variable the agent
// will read at runtime. Operators set the value via Airlock's UI; the
// agent reads its startup snapshot through the returned EnvVarHandle.
//
// Two flavours, distinguished by the Secret flag:
//   - Secret=false: plain config value (regions, hostnames, feature flags).
//     Operator sees and edits the current value in the UI. Not added to
//     the agent's redact set.
//   - Secret=true: credential. Operator can paste a value but cannot read
//     it back — only rotate. Added to the redact set before startup hooks
//     so substring matches are stripped from LLM input.
//
// Bytes by convention: base64-encode and decode in agent code. Single
// string per slug — for compound credentials register multiple slugs.
type EnvVar struct {
	noUnkeyedLiterals

	// Slug is the unique identifier per agent. Mirrored as the URL
	// segment in /api/v1/agents/{id}/env-vars/{slug}.
	Slug string

	// Description is shown to the operator in the editor UI. Never sent
	// to the LLM.
	Description string

	// Secret toggles the write-only UI affordance + redaction. See the
	// type doc for full semantics.
	Secret bool

	// Default is the value used when the operator hasn't configured the
	// slot. Lets an agent ship with sensible plain-config defaults
	// (region="us-east-1", timeout="30s") that the operator only
	// overrides when needed.
	//
	// Forbidden for Secret=true: there is no sensible default for a
	// credential, and a hardcoded one in agent source would defeat the
	// point of the secrets surface. RegisterEnvVar panics if both are
	// set.
	Default string

	// Pattern is an optional Go regex (RE2) the operator-supplied value
	// must match. Airlock rejects values that don't match at save time,
	// so typos in known-shape credentials (AWS keys, region codes,
	// hostnames) surface immediately rather than at first runtime use.
	// Empty string disables validation.
	//
	// Validated against agent's declaration (mirrors the Description
	// and Default fields), not against a per-set choice — operators
	// can't bypass the pattern.
	Pattern string
}

// EnvVarHandle is a compile-time binding to a registered EnvVar. Returned
// by RegisterEnvVar. Get reads the immutable process-start configuration.
// Configuration changes take effect when a replacement runtime becomes ready.
type EnvVarHandle struct {
	slug         string
	secret       bool
	defaultValue string
	pattern      *regexp.Regexp // nil when EnvVar.Pattern == ""
	agent        *Agent
}

// Get returns the operator-supplied value, falling back to the
// agent-declared Default when the operator hasn't set anything (always
// "" for secrets). For Secret=true vars, the value is registered with
// the agent's redact set so it's stripped from outbound
// LLM input.
//
// Return shape:
//   - (s, nil)        — the stored value (or Default if no value was set,
//     or "" if neither). Empty string IS a valid
//     successful return when no Pattern is declared.
//   - ("", non-nil)   — runtime/configuration unavailable, or the value does not
//     match the declared Pattern. Pattern is checked
//     unconditionally, including against empty
//     strings — declare Pattern="^.+$" to enforce
//     non-empty, or any tighter regex for a known
//     shape. Operators are blocked from saving a
//     non-matching value at the UI, so a mismatch
//     here usually means nothing has been configured
//     yet (or the Pattern was tightened after a
//     save).
//
// Every call reads the same startup snapshot; it performs no network I/O.
func (h *EnvVarHandle) Get(ctx context.Context) (string, error) {
	if !h.agent.runtimeAvailable() {
		return "", h.agent.runtimeUnavailable("EnvVarHandle.Get")
	}
	if h.agent.envConfig.Protocol != wire.EnvConfigProtocol {
		return "", errors.New("agentsdk: startup configuration unavailable; rebuild the app and host with compatible SDK support")
	}
	value, configured := h.agent.envConfig.Values[h.slug]
	if !configured {
		value = h.defaultValue
	}
	if h.pattern != nil && !h.pattern.MatchString(value) {
		return "", fmt.Errorf("agentsdk: env var %q value does not match Pattern %q", h.slug, h.pattern.String())
	}

	if h.secret {
		h.agent.maybeAddSensitive(value)
	}
	return value, nil
}

func (a *Agent) loadEnvConfig() error {
	raw := os.Getenv(wire.EnvConfigVariable)
	if raw == "" && len(a.envVars) == 0 {
		a.envConfig = wire.EnvConfigSnapshot{Protocol: wire.EnvConfigProtocol}
		return nil
	}
	var snapshot wire.EnvConfigSnapshot
	if json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.Protocol != wire.EnvConfigProtocol || snapshot.Revision < 1 || snapshot.Attempt == "" || snapshot.Values == nil {
		return errors.New("agentsdk: immutable startup configuration is required; rebuild the app and host with compatible SDK support")
	}
	if err := os.Unsetenv(wire.EnvConfigVariable); err != nil {
		return errors.New("agentsdk: cannot remove private configuration transport from the process environment")
	}
	a.envConfig = snapshot
	for slug, declaration := range a.envVars {
		if declaration.Secret {
			a.maybeAddSensitive(snapshot.Values[slug])
		}
	}
	return nil
}

// Slug returns the registered slug for this handle.
func (h *EnvVarHandle) Slug() string { return h.slug }

// IsSecret reports whether this var was registered as a secret.
func (h *EnvVarHandle) IsSecret() bool { return h.secret }
