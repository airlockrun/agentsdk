package main

import "testing"

func TestSelectBaseline(t *testing.T) {
	tests := []struct {
		name         string
		tags         []string
		current      string
		wantBaseline string
		wantBreaking bool
	}{
		{
			name:         "latest tag in current zero-major series",
			tags:         []string{"v0.3.0", "v0.4.0-rc.2", "v0.4.0-rc.10"},
			current:      "v0.4.0-rc.11",
			wantBaseline: "v0.4.0-rc.10",
		},
		{
			name:         "first release in new zero-major series",
			tags:         []string{"v0.3.0", "v0.4.9"},
			current:      "v0.5.0-rc.1",
			wantBaseline: "v0.4.9",
			wantBreaking: true,
		},
		{
			name:         "latest stable major series",
			tags:         []string{"v1.8.0", "v2.0.0-rc.1", "v2.0.0"},
			current:      "v2.1.0",
			wantBaseline: "v2.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline, breaking, err := selectBaseline(tt.tags, tt.current)
			if err != nil {
				t.Fatal(err)
			}
			if baseline != tt.wantBaseline || breaking != tt.wantBreaking {
				t.Fatalf("got (%q, %v), want (%q, %v)", baseline, breaking, tt.wantBaseline, tt.wantBreaking)
			}
		})
	}
}

func TestIgnoredMetadataChange(t *testing.T) {
	if !ignoredMetadataChange("github.com/airlockrun/agentsdk", "Version: value changed from 1 to 2") {
		t.Fatal("Version value changes must be ignored")
	}
	if ignoredMetadataChange("github.com/airlockrun/agentsdk", "Version: changed from untyped string to string") {
		t.Fatal("Version type changes must not be ignored")
	}
	if ignoredMetadataChange("github.com/airlockrun/agentsdk/agenttest", "Version: value changed from 1 to 2") {
		t.Fatal("metadata exceptions must be package-specific")
	}
}
