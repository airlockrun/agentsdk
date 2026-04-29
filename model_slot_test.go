package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegisterModel_Accumulates(t *testing.T) {
	a, _ := testAgent(t)

	a.RegisterModel(&ModelSlot{Slug: "summarize", Capability: CapText, Description: "Short summaries"})
	a.RegisterModel(&ModelSlot{Slug: "thumbnail", Capability: CapImage})

	if got := len(a.modelSlots); got != 2 {
		t.Fatalf("len(modelSlots) = %d, want 2", got)
	}
	if a.modelSlots[0].Slug != "summarize" || a.modelSlots[0].Capability != CapText {
		t.Errorf("slot[0] = %+v", a.modelSlots[0])
	}
	if a.modelSlots[0].Description != "Short summaries" {
		t.Errorf("slot[0].Description = %q", a.modelSlots[0].Description)
	}
	if a.modelSlots[1].Capability != CapImage {
		t.Errorf("slot[1].Capability = %q", a.modelSlots[1].Capability)
	}
}

func TestRegisterModel_PanicsOnEmptyCapability(t *testing.T) {
	a, _ := testAgent(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty Capability")
		}
	}()
	a.RegisterModel(&ModelSlot{Slug: "bad"})
}

func TestRegisterModel_SyncPayload(t *testing.T) {
	a, mock := testAgent(t)

	a.RegisterModel(&ModelSlot{Slug: "summarize", Capability: CapText, Description: "Short summaries"})
	a.RegisterModel(&ModelSlot{Slug: "poster", Capability: CapImage})

	a.syncWithAirlock(context.Background())

	reqs := mock.RequestsByPath("/api/agent/sync")
	if len(reqs) != 1 {
		t.Fatalf("sync requests = %d, want 1", len(reqs))
	}

	var body SyncRequest
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("decode sync body: %v", err)
	}
	if len(body.ModelSlots) != 2 {
		t.Fatalf("ModelSlots len = %d, want 2", len(body.ModelSlots))
	}
	if body.ModelSlots[0].Slug != "summarize" || body.ModelSlots[0].Capability != "text" {
		t.Errorf("slot[0] = %+v", body.ModelSlots[0])
	}
	if body.ModelSlots[1].Slug != "poster" || body.ModelSlots[1].Capability != "image" {
		t.Errorf("slot[1] = %+v", body.ModelSlots[1])
	}
}
