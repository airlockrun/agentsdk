package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAddInstruction_Accumulates(t *testing.T) {
	a, _ := testAgent(t)

	a.AddInstruction(&Instruction{Text: "all-access baseline"})
	a.AddInstruction(&Instruction{Text: "admin-only ops", Access: []Access{AccessAdmin}})
	a.AddInstruction(&Instruction{Text: "members only", Access: []Access{AccessAdmin, AccessUser}})

	if got := len(a.instructions); got != 3 {
		t.Fatalf("len(instructions) = %d, want 3", got)
	}

	if a.instructions[0].Text != "all-access baseline" {
		t.Errorf("specs[0].Text = %q", a.instructions[0].Text)
	}
	if len(a.instructions[0].Access) != 0 {
		t.Errorf("specs[0].Access = %v, want empty (= all levels)", a.instructions[0].Access)
	}

	if len(a.instructions[1].Access) != 1 || a.instructions[1].Access[0] != AccessAdmin {
		t.Errorf("specs[1].Access = %v, want [admin]", a.instructions[1].Access)
	}

	if len(a.instructions[2].Access) != 2 {
		t.Errorf("specs[2].Access len = %d, want 2", len(a.instructions[2].Access))
	}
}

func TestAddInstruction_SyncPayload(t *testing.T) {
	a, mock := testAgent(t)

	a.AddInstruction(&Instruction{Text: "hello everyone"})
	a.AddInstruction(&Instruction{Text: "hello admin", Access: []Access{AccessAdmin}})

	a.syncWithAirlock(context.Background())

	reqs := mock.RequestsByPath("/api/agent/sync")
	if len(reqs) != 1 {
		t.Fatalf("sync requests = %d, want 1", len(reqs))
	}

	var body SyncRequest
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("decode sync body: %v", err)
	}

	if len(body.Instructions) != 2 {
		t.Fatalf("sync Instructions len = %d, want 2", len(body.Instructions))
	}
	if body.Instructions[0].Text != "hello everyone" || len(body.Instructions[0].Access) != 0 {
		t.Errorf("first extra wrong: %+v", body.Instructions[0])
	}
	if body.Instructions[1].Text != "hello admin" ||
		len(body.Instructions[1].Access) != 1 ||
		body.Instructions[1].Access[0] != AccessAdmin {
		t.Errorf("second extra wrong: %+v", body.Instructions[1])
	}
}
