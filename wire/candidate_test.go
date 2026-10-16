package wire

import (
	"encoding/json"
	"testing"
)

func TestCandidatePlanIsBoundedExplicitAndCredentialFree(t *testing.T) {
	plan := CandidateConfig{Version: 1, Selections: []CandidateSelection{{Type: "connection", Slug: "crm", FixtureName: "canned-crm"}, {Type: "mcp_server", Slug: "docs", ResourceID: "11111111-1111-1111-1111-111111111111"}}}
	value, err := plan.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["version"] != float64(1) || len(raw["selections"].([]any)) != 2 {
		t.Fatalf("header=%s", value)
	}
	for _, bad := range []CandidateSelection{{Type: "connection", Slug: "crm"}, {Type: "connection", Slug: "crm", FixtureName: "f", ResourceID: "11111111-1111-1111-1111-111111111111"}, {Type: "connection", Slug: "../escape", FixtureName: "f"}, {Type: "connector", Slug: "x", FixtureName: "f"}, {Type: "mcp_server", Slug: "x", FixtureName: "f"}, {Type: "connection", Slug: "x", ResourceID: "00000000-0000-0000-0000-000000000000"}} {
		if _, err := (CandidateConfig{Version: 1, Selections: []CandidateSelection{bad}}).HeaderValue(); err == nil {
			t.Fatalf("accepted invalid selection %+v", bad)
		}
	}
	plan.Selections = append(plan.Selections, plan.Selections[0])
	if _, err := plan.HeaderValue(); err == nil {
		t.Fatal("duplicate slot accepted")
	}
}
