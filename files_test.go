package agentsdk

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFilePathSchemaMarker(t *testing.T) {
	type In struct {
		File FilePath `json:"file"`
		Dir  DirPath  `json:"dir"`
		Name string   `json:"name"`
	}
	type Out struct {
		Result FilePath `json:"result"`
	}

	a, _ := testAgent(t)
	a.RegisterTool(&Tool[In, Out]{
		Name:        "demo",
		Description: "demo",
		Execute: func(ctx context.Context, in In) (Out, error) {
			return Out{Result: FilePath("ok")}, nil
		},
	})

	rt := a.tools["demo"]
	if rt == nil {
		t.Fatal("tool not registered")
	}

	inSchema, _ := json.Marshal(rt.InputSchema)
	got := string(inSchema)
	for _, want := range []string{`"format":"agent-file"`, `"format":"agent-dir"`} {
		if !contains(got, want) {
			t.Errorf("input schema missing %s\n%s", want, got)
		}
	}
	// Plain string field must NOT acquire a format marker.
	if contains(got, `"name":{"format"`) {
		t.Errorf("plain string field unexpectedly got format marker: %s", got)
	}

	outSchema, _ := json.Marshal(rt.OutputSchema)
	if !contains(string(outSchema), `"format":"agent-file"`) {
		t.Errorf("output schema missing agent-file marker:\n%s", string(outSchema))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
