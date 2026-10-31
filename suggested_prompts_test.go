package agentsdk

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestRegisterSuggestedPromptManifest(t *testing.T) {
	a := New(Config{Description: "Catalogue"})
	want := []SuggestedPrompt{{Title: "Find a product", Prompt: "Help me find a product in the catalogue."}, {Title: "Найти товар", Prompt: "Помоги найти товар."}}
	for _, p := range want {
		a.RegisterSuggestedPrompt(p)
	}
	manifest := a.Manifest()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var got wire.AgentManifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SuggestedPrompts, want) {
		t.Fatalf("roundtrip = %#v", got.SuggestedPrompts)
	}
	manifest.SuggestedPrompts[0].Title = "Changed"
	if a.Manifest().SuggestedPrompts[0].Title != want[0].Title {
		t.Fatal("manifest aliases registration")
	}
	expectPanicContains(t, "RegisterSuggestedPrompt", func() { a.RegisterSuggestedPrompt(want[0]) })
}

func TestRegisterSuggestedPromptInvalid(t *testing.T) {
	a := New(Config{Description: "Catalogue"})
	expectPanicContains(t, "title", func() { a.RegisterSuggestedPrompt(SuggestedPrompt{Prompt: "Find"}) })
	for range wire.MaxSuggestedPrompts {
		a.RegisterSuggestedPrompt(SuggestedPrompt{Title: "Find", Prompt: "Find a product."})
	}
	expectPanicContains(t, "at most", func() { a.RegisterSuggestedPrompt(SuggestedPrompt{Title: "Find", Prompt: "Find a product."}) })
}
