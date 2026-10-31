package agentsdk

import (
	"strings"
	"testing"
)

func TestUpgradeInstructions(t *testing.T) {
	tests := []struct {
		name, from, to string
		wantContains   string
		wantEmpty      bool
		wantErr        string
	}{
		{name: "cross prerelease boundary", from: "0.8.1-alpha.6", to: "0.8.1", wantContains: "## v0.8.1"},
		{name: "leading v and build metadata", from: "v0.8.1-alpha.6+internal", to: "v0.8.1+build.4", wantContains: "hx-disabled-elt"},
		{name: "storage declaration migration", from: "0.8.1-alpha.7", to: "0.8.1", wantContains: "Declare every app-storage prefix"},
		{name: "canonical mock task migration", from: "0.8.1-alpha.8", to: "0.8.1", wantContains: "goai/testutil.NewMockModel"},
		{name: "submission review migration", from: "0.8.1-alpha.11", to: "0.8.1", wantContains: "AgentDefinition.Review"},
		{name: "target before consolidated release", from: "0.8.1-alpha.7", to: "0.8.1-alpha.12", wantEmpty: true},
		{name: "lazy route migration", from: "0.8.1-alpha.12", to: "0.8.1", wantContains: "LoggingNormal"},
		{name: "environment snapshot migration", from: "0.8.1-alpha.12", to: "0.8.1", wantContains: "EnvVarHandle.Refresh()"},
		{name: "SQL capability migration", from: "0.8.1-alpha.12", to: "0.8.1", wantContains: "agentsdk.CapabilityQueryDB"},
		{name: "app icon migration", from: "0.8.1-alpha.12", to: "0.8.1", wantContains: "agentsdk.Config.Icon"},
		{name: "already at latest entry", from: "0.8.1", to: "0.8.2", wantEmpty: true},
		{name: "same version", from: "0.8.1", to: "v0.8.1", wantEmpty: true},
		{name: "same-base content addressed prerelease", from: "0.8.1-devabc123", to: "0.8.1", wantContains: "## v0.8.1"},
		{name: "invalid source", from: "", to: "0.8.1", wantErr: "invalid source SDK version"},
		{name: "invalid target", from: "0.8.0", to: "latest", wantErr: "invalid target SDK version"},
		{name: "stable to prerelease downgrade", from: "0.8.1", to: "0.8.1-alpha.8", wantErr: "cannot select upgrade instructions for downgrade"},
		{name: "downgrade", from: "0.9.0", to: "0.8.1", wantErr: "cannot select upgrade instructions for downgrade"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UpgradeInstructions(tt.from, tt.to)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("UpgradeInstructions() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantEmpty && got != "" {
				t.Fatalf("UpgradeInstructions() = %q, want empty", got)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Fatalf("UpgradeInstructions() missing %q:\n%s", tt.wantContains, got)
			}
		})
	}
}

func TestConsolidatedStableUpgradeInstructions(t *testing.T) {
	entries, err := parseUpgradeInstructions(upgradeInstructionsMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].version != "v0.8.1" {
		t.Fatalf("expected one consolidated v0.8.1 entry, got %#v", entries)
	}
	for _, from := range []string{"0.7.0", "0.8.1-alpha.7", "0.8.1-alpha.8", "0.8.1-alpha.9", "0.8.1-alpha.12", "0.8.1-alpha.13"} {
		t.Run(from, func(t *testing.T) {
			got, err := UpgradeInstructions(from, "0.8.1")
			if err != nil {
				t.Fatal(err)
			}
			if got != entries[0].text {
				t.Fatal("source version did not receive the complete consolidated migration")
			}
			for _, required := range []string{
				"EnvVarHandle.Refresh()", "go tool air resources", "agentsdk.Config.Icon", "LoggingNormal", "agentsdk.CapabilityQueryDB",
				"RegisterDirectory", "AccessInternal", "hx-ignore", "hx-disabled-elt", "event.detail.ctx.response.status",
				"goai/testutil.NewMockModel", "MockConfig.Stream", "agentsdk/testutil.WithCaller", "remote.TestClientFromEnvironment()",
				"AgentDefinition.Review", "checkpoint version 2", "SaveCheckpoint", "executable identity",
			} {
				if !strings.Contains(got, required) {
					t.Errorf("consolidated migration missing %q", required)
				}
			}
			for _, unavailable := range []string{"agenttest.Options.Models", "agenttest.RunAgent", "agenttest.RegisterFlags", "env.MockModel(slot)", "-agenttest.model="} {
				if strings.Contains(got, unavailable) {
					t.Errorf("consolidated migration instructs use of unavailable API %q", unavailable)
				}
			}
		})
	}
}

func TestParseUpgradeInstructions(t *testing.T) {
	markdown := `# Guide

Intro before entries.

## v2.0.0

Second.

~~~markdown
## v9.0.0
~~~

## v1.0.0

First.

### Detail

More.
`
	entries, err := parseUpgradeInstructions(markdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].version != "v1.0.0" || entries[1].version != "v2.0.0" {
		t.Fatalf("entries = %#v", entries)
	}
	if !strings.Contains(entries[1].text, "## v9.0.0") {
		t.Fatalf("fenced heading was not retained in the body:\n%s", entries[1].text)
	}
	selected, err := selectUpgradeInstructions(markdown, "v0.9.0", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if first, second := strings.Index(selected, "## v1.0.0"), strings.Index(selected, "## v2.0.0"); first < 0 || second < 0 || first >= second {
		t.Fatalf("selected entries are not ascending:\n%s", selected)
	}

	tests := []struct {
		name, markdown, want string
	}{
		{name: "malformed heading", markdown: "## release-1\n\nDo it.\n", want: "malformed version heading"},
		{name: "bare heading", markdown: "## 1.2.3\n\nA.\n", want: "malformed version heading"},
		{name: "indented heading", markdown: "  ## v1.2.3\n\nA.\n", want: "malformed version heading"},
		{name: "duplicate normalized", markdown: "## v1.2.3\n\nA.\n## v1.2.3\n\nB.\n", want: "duplicate upgrade version"},
		{name: "duplicate precedence", markdown: "## v1.2.3+one\n\nA.\n## v1.2.3+two\n\nB.\n", want: "duplicate upgrade version precedence"},
		{name: "empty entry", markdown: "## v1.2.3\n\n## v1.2.4\n\nB.\n", want: "has no instructions"},
		{name: "unterminated fence", markdown: "## v1.2.3\n\n```go\n## v2.0.0\n", want: "unterminated fenced code block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseUpgradeInstructions(tt.markdown)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseUpgradeInstructions() error = %v, want %q", err, tt.want)
			}
		})
	}
}
