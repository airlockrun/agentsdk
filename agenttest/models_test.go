package agenttest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/session"
)

func modelManifest() *agentsdk.Agent {
	a := agentsdk.New(agentsdk.Config{Description: "Model configuration"})
	for slug, capability := range map[string]agentsdk.ModelCapability{"summary": agentsdk.CapText, "ocr": agentsdk.CapVision, "image": agentsdk.CapImage} {
		a.RegisterModel(&agentsdk.ModelSlot{Slug: slug, Capability: capability, Description: slug})
	}
	return a
}

func prepareModelEnv(t *testing.T, options Options, resolver modelResolver) *Env {
	t.Helper()
	models, err := prepareModels(t.Context(), modelManifest().Manifest(), options, resolver)
	if err != nil {
		t.Fatal(err)
	}
	mock, url := newMockWithModels(models)
	t.Cleanup(mock.Close)
	return &Env{Airlock: mock, URL: url, models: models}
}

func modelText(t *testing.T, model stream.Model, prompt string) string {
	t.Helper()
	ch, err := model.Stream(t.Context(), &stream.CallOptions{Messages: []message.Message{message.NewUserMessage(prompt)}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for event := range ch {
		if failure, ok := event.Data.(stream.ErrorEvent); ok {
			t.Fatal(failure.Error)
		}
		if delta, ok := event.Data.(stream.TextDeltaEvent); ok {
			text += delta.Text
		}
	}
	return text
}

func TestModelsDefaultMocks(t *testing.T) {
	env := prepareModelEnv(t, Options{}, func(context.Context, string) (stream.Model, session.ModelLimits, error) {
		t.Fatal("ordinary mock setup called live resolver")
		return nil, session.ModelLimits{}, errors.New("unexpected resolver")
	})
	for _, slot := range []string{"", "summary", "ocr"} {
		model, limits, err := env.modelForSlot(slot)
		if err != nil {
			t.Fatal(err)
		}
		if model != env.MockModel(slot) || limits.Context <= 0 {
			t.Fatalf("slot %q model/limits = %v, %+v", slot, model, limits)
		}
		if text := modelText(t, model, "anything"); text != "Hello" {
			t.Fatalf("slot %q text = %q", slot, text)
		}
	}
	for _, slot := range []string{"missing", "image"} {
		if _, _, err := env.modelForSlot(slot); err == nil {
			t.Fatalf("slot %q lookup succeeded", slot)
		}
	}
}

func TestModelsConfigureMatchingAndIsolation(t *testing.T) {
	env := prepareModelEnv(t, Options{}, nil)
	other := prepareModelEnv(t, Options{}, nil)
	mock := env.MockModel("summary")
	match := "specific request"
	if err := mock.Configure(testutil.MockConfig{ID: mock.ID(), Default: &testutil.MockResponse{Text: "any request"}, Rules: []testutil.MockRule{{LastUserText: &match, Response: testutil.MockResponse{Text: "specific response"}}}}); err != nil {
		t.Fatal(err)
	}
	match = "mutated"
	model := proxy.Model("", proxy.Options{BaseURL: env.URL, Slug: "summary", Capability: "text"})
	for prompt, want := range map[string]string{"specific request": "specific response", "other": "any request"} {
		if got := modelText(t, model, prompt); got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
	}
	if len(mock.Requests()) != 2 {
		t.Fatalf("requests = %d", len(mock.Requests()))
	}
	if got := modelText(t, env.MockModel(""), "specific request"); got != "Hello" {
		t.Fatalf("default changed: %q", got)
	}
	if got := modelText(t, other.MockModel("summary"), "specific request"); got != "Hello" {
		t.Fatalf("other environment changed: %q", got)
	}
}

func TestModelsValidateBeforeResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
		want    string
	}{
		{"unknown live slot", Options{Models: map[string]string{"summary": "work/openai/gpt-5.4", "unknown": "work/openai/gpt-5.4"}}, "not registered"},
		{"non-streaming live", Options{Models: map[string]string{"image": "work/openai/gpt-5.4"}}, "non-streaming"},
		{"unknown mock slot", Options{Mocks: map[string]testutil.MockConfig{"unknown": {}}}, "not registered"},
		{"non-streaming mock", Options{Mocks: map[string]testutil.MockConfig{"image": {}}}, "non-streaming"},
		{"missing account", Options{Models: map[string]string{"summary": "openai/gpt-5.4"}}, "slug/provider/model"},
		{"malformed model", Options{Models: map[string]string{"summary": "gpt-5.4"}}, "slug/provider/model"},
		{"invalid mock", Options{Mocks: map[string]testutil.MockConfig{"summary": {Rules: []testutil.MockRule{{}}}}}, "matcher"},
		{"unknown limits", Options{Limits: map[string]session.ModelLimits{"unknown": {Context: 1000}}}, "not registered"},
		{"invalid limits", Options{Limits: map[string]session.ModelLimits{"summary": {Context: -1}}}, "negative"},
		{"live limit override", Options{Models: map[string]string{"summary": "work/openai/gpt-5.4"}, Limits: map[string]session.ModelLimits{"summary": {Context: 1000}}}, "resolved by the provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareModels(t.Context(), modelManifest().Manifest(), tc.options, func(context.Context, string) (stream.Model, session.ModelLimits, error) {
				t.Error("invalid options reached resolver")
				return nil, session.ModelLimits{}, errors.New("unexpected call")
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestModelsLiveSelectionKeepsDormantMock(t *testing.T) {
	live, err := testutil.NewMockModel(testutil.MockConfig{ID: "resolved", Default: &testutil.MockResponse{Text: "resolved response"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	env := prepareModelEnv(t, Options{Models: map[string]string{"summary": "work/openai/gpt-5.4"}}, func(ctx context.Context, name string) (stream.Model, session.ModelLimits, error) {
		calls++
		if ctx == nil || name != "work/openai/gpt-5.4" {
			t.Fatalf("resolver arguments = %v, %q", ctx, name)
		}
		return live, session.ModelLimits{Context: 200000, Output: 8192}, nil
	})
	dormant := env.MockModel("summary")
	if err := dormant.Configure(testutil.MockConfig{ID: dormant.ID(), Default: &testutil.MockResponse{Text: "mock response"}}); err != nil {
		t.Fatal(err)
	}
	model, limits, err := env.modelForSlot("summary")
	if err != nil {
		t.Fatal(err)
	}
	if model != live || limits.Context != 200000 || calls != 1 {
		t.Fatalf("live lookup = %v %+v; calls=%d", model, limits, calls)
	}
	if got := modelText(t, proxy.Model("", proxy.Options{BaseURL: env.URL, Slug: "summary", Capability: "text"}), "request"); got != "resolved response" {
		t.Fatalf("live text = %q", got)
	}
	if len(dormant.Requests()) != 0 {
		t.Fatal("live call reached dormant mock")
	}
	if got := modelText(t, proxy.Model("", proxy.Options{BaseURL: env.URL, Capability: "text"}), "request"); got != "Hello" {
		t.Fatalf("default mock text = %q", got)
	}
	if model, _, _ := env.modelForSlot(""); model != env.MockModel("") {
		t.Fatal("unselected default switched live")
	}
}

func TestModelsResolverErrors(t *testing.T) {
	var typedNil *testutil.MockModel
	for _, tc := range []struct {
		name   string
		model  stream.Model
		limits session.ModelLimits
		err    error
		want   string
	}{
		{"missing credentials", nil, session.ModelLimits{}, errors.New("missing credentials"), "missing credentials"},
		{"nil model", nil, session.ModelLimits{Context: 1000}, nil, "nil model"},
		{"typed nil", typedNil, session.ModelLimits{Context: 1000}, nil, "nil model"},
		{"missing limits", liveMock(t), session.ModelLimits{}, nil, "positive input budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareModels(t.Context(), modelManifest().Manifest(), Options{Models: map[string]string{"summary": "work/openai/gpt-5.4"}}, func(context.Context, string) (stream.Model, session.ModelLimits, error) {
				return tc.model, tc.limits, tc.err
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func liveMock(t *testing.T) *testutil.MockModel {
	t.Helper()
	model, err := testutil.NewMockModel(testutil.MockConfig{ID: "resolved"})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestModelsDefaultWithoutDeclaredSlots(t *testing.T) {
	a := agentsdk.New(agentsdk.Config{Description: "default only"})
	models, err := prepareModels(t.Context(), a.Manifest(), Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[""].mock == nil {
		t.Fatalf("models = %+v", models)
	}
}
