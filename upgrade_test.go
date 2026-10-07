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
		{name: "cross prerelease boundary", from: "0.8.1-alpha.6", to: "0.8.1", wantContains: "## v0.8.1-alpha.7"},
		{name: "leading v and build metadata", from: "v0.8.1-alpha.6+internal", to: "v0.8.1+build.4", wantContains: "hx-disabled-elt"},
		{name: "already at entry", from: "0.8.1-alpha.7", to: "0.8.1", wantEmpty: true},
		{name: "same version", from: "0.8.1", to: "v0.8.1", wantEmpty: true},
		{name: "same-base content addressed prerelease", from: "0.8.1-devabc123", to: "0.8.1", wantEmpty: true},
		{name: "invalid source", from: "", to: "0.8.1", wantErr: "invalid source SDK version"},
		{name: "invalid target", from: "0.8.0", to: "latest", wantErr: "invalid target SDK version"},
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
