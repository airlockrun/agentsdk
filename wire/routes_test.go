package wire

import "testing"

func TestValidateRouteTools(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tool      string
		wantError bool
	}{
		{"unlinked", "", false},
		{"registered internal tool", "save", false},
		{"unknown tool", "other_app_tool", true},
		{"noncanonical name", " save", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := AgentManifest{
				Tools:  []ToolDef{{Name: "save", Access: AccessInternal}},
				Routes: []RouteDef{{Method: "POST", Path: "/save", Access: AccessPublic, ToolName: tc.tool}},
			}
			if err := ValidateRouteTools(manifest); (err != nil) != tc.wantError {
				t.Fatalf("ValidateRouteTools = %v; want error=%v", err, tc.wantError)
			}
		})
	}
}
