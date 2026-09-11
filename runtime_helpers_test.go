package agentsdk

import (
	"testing"

	"github.com/airlockrun/agentsdk/capability"
)

func TestConvertLineEdits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     []capability.LineEditInput
		wantError bool
	}{
		{"empty", nil, true},
		{"invalid line", []capability.LineEditInput{{From: 0, Text: "x"}}, true},
		{"negative count", []capability.LineEditInput{{From: 1, Count: -1}}, true},
		{"empty insert", []capability.LineEditInput{{From: 1}}, true},
		{"insert", []capability.LineEditInput{{From: 1, Text: "x"}}, false},
		{"delete", []capability.LineEditInput{{From: 1, Count: 2}}, false},
		{"replace", []capability.LineEditInput{{From: 1, Count: 2, Text: "x"}}, false},
		{"append", []capability.LineEditInput{{Append: "x"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edits, err := convertLineEdits(tc.input)
			if (err != nil) != tc.wantError {
				t.Fatalf("edits=%+v error=%v", edits, err)
			}
			if !tc.wantError && len(edits) != len(tc.input) {
				t.Fatal("lost edits")
			}
		})
	}
}
