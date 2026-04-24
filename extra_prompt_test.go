package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAddExtraPrompt_Accumulates(t *testing.T) {
	a, _ := testAgent(t)

	a.AddExtraPrompt("all-access baseline")
	a.AddExtraPrompt("admin-only ops", AccessAdmin)
	a.AddExtraPrompt("members only", AccessAdmin, AccessUser)

	if got := len(a.extraPrompts); got != 3 {
		t.Fatalf("len(extraPrompts) = %d, want 3", got)
	}

	if a.extraPrompts[0].Text != "all-access baseline" {
		t.Errorf("specs[0].Text = %q", a.extraPrompts[0].Text)
	}
	if len(a.extraPrompts[0].Access) != 0 {
		t.Errorf("specs[0].Access = %v, want empty (= all levels)", a.extraPrompts[0].Access)
	}

	if len(a.extraPrompts[1].Access) != 1 || a.extraPrompts[1].Access[0] != AccessAdmin {
		t.Errorf("specs[1].Access = %v, want [admin]", a.extraPrompts[1].Access)
	}

	if len(a.extraPrompts[2].Access) != 2 {
		t.Errorf("specs[2].Access len = %d, want 2", len(a.extraPrompts[2].Access))
	}
}

func TestAddExtraPrompt_SyncPayload(t *testing.T) {
	a, mock := testAgent(t)

	a.AddExtraPrompt("hello everyone")
	a.AddExtraPrompt("hello admin", AccessAdmin)

	a.syncWithAirlock(context.Background())

	reqs := mock.RequestsByPath("/api/agent/sync")
	if len(reqs) != 1 {
		t.Fatalf("sync requests = %d, want 1", len(reqs))
	}

	var body SyncRequest
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("decode sync body: %v", err)
	}

	if len(body.ExtraPrompts) != 2 {
		t.Fatalf("sync ExtraPrompts len = %d, want 2", len(body.ExtraPrompts))
	}
	if body.ExtraPrompts[0].Text != "hello everyone" || len(body.ExtraPrompts[0].Access) != 0 {
		t.Errorf("first extra wrong: %+v", body.ExtraPrompts[0])
	}
	if body.ExtraPrompts[1].Text != "hello admin" ||
		len(body.ExtraPrompts[1].Access) != 1 ||
		body.ExtraPrompts[1].Access[0] != AccessAdmin {
		t.Errorf("second extra wrong: %+v", body.ExtraPrompts[1])
	}
}
