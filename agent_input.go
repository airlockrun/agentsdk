package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Input returns a fresh decoded copy of this session's original admitted Start
// input. It is available only in this handle's authenticated private callback.
// Continue prompts never replace it. No network or app-owned task storage is used.
func (h *AgentHandle[In, Out]) Input(ctx context.Context) (In, error) {
	var out In
	if ctx == nil {
		return out, errors.New("agentsdk: task callback context is required")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	d := h.registeredAgent().definition
	s, ok := ctx.Value(taskInputCtxKey{}).(taskInputSnapshot)
	r := runFromContext(ctx)
	if !ok || r == nil || s.agent != h.agent || r.agent != h.agent || s.runID != r.id || s.definition.Slug != d.Slug || s.definition.ContractHash != d.ContractHash || len(s.input) == 0 {
		return out, errors.New("agentsdk: original input unavailable outside the matching task callback")
	}
	if err := validateAgentTypeValue(reflect.TypeFor[In](), s.input); err != nil {
		return out, fmt.Errorf("agentsdk: invalid original task input: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(s.input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, fmt.Errorf("agentsdk: decode original task input: %w", err)
	}
	return out, nil
}
