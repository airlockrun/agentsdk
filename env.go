package agentsdk

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

// EnvVar declares an operator-configured environment variable the agent
// will read at runtime. Operators set the value via Airlock's UI; the
// agent fetches it through the returned EnvVarHandle.
//
// Two flavours, distinguished by the Secret flag:
//   - Secret=false: plain config value (regions, hostnames, feature flags).
//     Operator sees and edits the current value in the UI. Not added to
//     the agent's redact set.
//   - Secret=true: credential. Operator can paste a value but cannot read
//     it back — only rotate. Auto-added to the redact set on first Get()
//     so substring matches are stripped from LLM input.
//
// Bytes by convention: base64-encode and decode in agent code. Single
// string per slug — for compound credentials register multiple slugs.
type EnvVar struct {
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
// by RegisterEnvVar; the agent calls Get(ctx) at runtime to fetch the
// operator-supplied value. Values are cached on the handle for the
// lifetime of the agent process — call Refresh() to force a re-fetch
// (e.g. after the operator rotates the value).
type EnvVarHandle struct {
	slug    string
	secret  bool
	pattern *regexp.Regexp // nil when EnvVar.Pattern == ""
	agent   *Agent

	mu     sync.Mutex
	cached string
	loaded bool
}

// Get returns the operator-supplied value, falling back to the
// agent-declared Default when the operator hasn't set anything (always
// "" for secrets). For Secret=true vars, the value is registered with
// the agent's redact set on each fetch so it's stripped from outbound
// LLM input.
//
// Return shape:
//   - (s, nil)        — the stored value (or Default if no value was set,
//                        or "" if neither). Empty string IS a valid
//                        successful return when no Pattern is declared.
//   - ("", non-nil)   — transport / decrypt error, or the value does not
//                        match the declared Pattern. Pattern is checked
//                        unconditionally, including against empty
//                        strings — declare Pattern="^.+$" to enforce
//                        non-empty, or any tighter regex for a known
//                        shape. Operators are blocked from saving a
//                        non-matching value at the UI, so a mismatch
//                        here usually means nothing has been configured
//                        yet (or the Pattern was tightened after a
//                        save).
//
// Subsequent calls return the cached value until Refresh() is invoked.
func (h *EnvVarHandle) Get(ctx context.Context) (string, error) {
	h.mu.Lock()
	if h.loaded {
		v := h.cached
		h.mu.Unlock()
		return v, nil
	}
	h.mu.Unlock()

	var resp EnvVarValueResponse
	if err := h.agent.client.doJSON(ctx, "GET", "/api/agent/env-vars/"+h.slug, nil, &resp); err != nil {
		return "", fmt.Errorf("agentsdk: get env var %q: %w", h.slug, err)
	}

	if h.pattern != nil && !h.pattern.MatchString(resp.Value) {
		return "", fmt.Errorf("agentsdk: env var %q value does not match Pattern %q", h.slug, h.pattern.String())
	}

	if h.secret {
		h.agent.maybeAddSensitive(resp.Value)
	}

	h.mu.Lock()
	h.cached = resp.Value
	h.loaded = true
	h.mu.Unlock()

	return resp.Value, nil
}

// Refresh discards the cached value so the next Get() re-fetches.
// Useful when the operator has rotated the value mid-run.
func (h *EnvVarHandle) Refresh() {
	h.mu.Lock()
	h.cached = ""
	h.loaded = false
	h.mu.Unlock()
}

// Slug returns the registered slug for this handle.
func (h *EnvVarHandle) Slug() string { return h.slug }

// IsSecret reports whether this var was registered as a secret.
func (h *EnvVarHandle) IsSecret() bool { return h.secret }
