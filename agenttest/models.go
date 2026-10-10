package agenttest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/airlockrun/agentsdk/internal/mockairlock"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/localconfig"
	"github.com/airlockrun/sol/session"
)

type modelSlot struct {
	model  stream.Model
	mock   *testutil.MockModel
	limits session.ModelLimits
}

type modelResolver func(context.Context, string) (stream.Model, session.ModelLimits, error)

// MockModel returns the slot's configurable in-process mock. An empty slot names
// the default text-generation model. Live slots retain a dormant mock so the same
// test setup can configure responses in both mocked and live runs. Panics for an
// unknown or non-streaming slot.
func (e *Env) MockModel(slot string) *testutil.MockModel {
	if e == nil {
		panic("agenttest: MockModel requires an environment")
	}
	selected, ok := e.models[slot]
	if !ok {
		panic(fmt.Sprintf("agenttest: streaming model slot %q is not registered", slot))
	}
	return selected.mock
}

// modelForSlot is shared by the local task runner and app model proxy bindings.
func (e *Env) modelForSlot(slot string) (stream.Model, session.ModelLimits, error) {
	if e == nil {
		return nil, session.ModelLimits{}, errors.New("agenttest: model lookup requires an environment")
	}
	selected, ok := e.models[slot]
	if !ok {
		return nil, session.ModelLimits{}, fmt.Errorf("agenttest: streaming model slot %q is not registered", slot)
	}
	return selected.model, selected.limits, nil
}

func prepareModels(ctx context.Context, manifest wire.AgentManifest, options Options, resolve modelResolver) (map[string]modelSlot, error) {
	slots := map[string]bool{"": true}
	for _, slot := range manifest.ModelSlots {
		slots[slot.Slug] = slot.Capability == "text" || slot.Capability == "vision"
	}
	// Validate every selection before any provider/credential resolution.
	for _, slug := range sortedModelKeys(options.Models) {
		if err := checkStreamingSlot(slots, slug); err != nil {
			return nil, err
		}
		if err := checkModelName(options.Models[slug]); err != nil {
			return nil, err
		}
	}
	for _, slug := range sortedModelKeys(options.Mocks) {
		if err := checkStreamingSlot(slots, slug); err != nil {
			return nil, err
		}
	}
	for _, slug := range sortedModelKeys(options.Limits) {
		if err := checkStreamingSlot(slots, slug); err != nil {
			return nil, err
		}
		if _, live := options.Models[slug]; live {
			return nil, fmt.Errorf("agenttest: live model limits for slot %q are resolved by the provider", slug)
		}
		if err := options.Limits[slug].Validate(true); err != nil {
			return nil, fmt.Errorf("agenttest: slot %q limits: %w", slug, err)
		}
	}
	models := make(map[string]modelSlot)
	for _, slug := range sortedModelKeys(slots) {
		if !slots[slug] {
			continue
		}
		id := "agenttest/" + slug
		if slug == "" {
			id = "agenttest/default"
		}
		config, configured := options.Mocks[slug]
		if !configured {
			config = testutil.MockConfig{ID: id, Default: &testutil.MockResponse{Text: "Hello", Usage: stream.UsageFrom(10, 5)}}
		} else if config.ID == "" {
			config.ID = id
		}
		mock, err := testutil.NewMockModel(config)
		if err != nil {
			return nil, fmt.Errorf("agenttest: slot %q mock: %w", slug, err)
		}
		limits := session.ModelLimits{Context: 100000, Output: 4096}
		if configured, ok := options.Limits[slug]; ok {
			limits = configured
		}
		models[slug] = modelSlot{model: mock, mock: mock, limits: limits}
	}
	for _, slug := range sortedModelKeys(options.Models) {
		if resolve == nil {
			return nil, errors.New("agenttest: live model resolver is required")
		}
		model, limits, err := resolve(ctx, options.Models[slug])
		if err != nil {
			return nil, fmt.Errorf("agenttest: resolve slot %q: %w", slug, err)
		}
		if model == nil || nilModel(model) {
			return nil, fmt.Errorf("agenttest: resolver returned nil model for slot %q", slug)
		}
		if err := limits.Validate(true); err != nil {
			return nil, fmt.Errorf("agenttest: resolved slot %q limits: %w", slug, err)
		}
		selected := models[slug]
		selected.model, selected.limits = model, limits
		models[slug] = selected
	}
	return models, nil
}

func checkStreamingSlot(slots map[string]bool, slug string) error {
	streaming, ok := slots[slug]
	if !ok {
		return fmt.Errorf("agenttest: model slot %q is not registered", slug)
	}
	if !streaming {
		return fmt.Errorf("agenttest: model slot %q has a non-streaming capability", slug)
	}
	return nil
}

func checkModelName(name string) error {
	_, err := localconfig.ParseModelRef(name)
	return err
}

func sortedModelKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nilModel(model stream.Model) bool {
	v := reflect.ValueOf(model)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func newMockWithModels(models map[string]modelSlot) (*MockAirlock, string) {
	bindings := make(map[string]stream.Model, len(models))
	for slug, selected := range models {
		bindings[slug] = selected.model
	}
	inner, url := mockairlock.NewWithModels(bindings)
	return &MockAirlock{Server: inner.Server, mock: inner}, url
}
