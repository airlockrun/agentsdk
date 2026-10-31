package wire

import "testing"

func TestValidateRouteLogging(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tool      string
		wantError bool
	}{
		{"default", "", false},
		{"normal", "normal", false},
		{"off", "off", false},
		{"unknown", "invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := AgentManifest{
				Tools:  []ToolDef{{Name: "save", Access: AccessInternal}},
				Routes: []RouteDef{{Method: "POST", Path: "/save", Access: AccessPublic, Logging: RouteLogging(tc.tool)}},
			}
			if err := ValidateRouteLogging(manifest); (err != nil) != tc.wantError {
				t.Fatalf("ValidateRouteTools = %v; want error=%v", err, tc.wantError)
			}
		})
	}
}

func TestRouteLoggingMethodDefaults(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		t.Run(method, func(t *testing.T) {
			want := LoggingNormal
			if method == "GET" || method == "HEAD" {
				want = LoggingOff
			}
			if got, err := LoggingDefault.Resolve(method); err != nil || got != want {
				t.Fatalf("default=%q %v want %q", got, err, want)
			}
			for _, explicit := range []RouteLogging{LoggingOff, LoggingNormal} {
				if got, err := explicit.Resolve(method); err != nil || got != explicit {
					t.Fatalf("explicit=%q %v", got, err)
				}
			}
		})
	}
}
