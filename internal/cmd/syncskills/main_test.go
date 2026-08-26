package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/lucide"
)

func TestWriteLucideIconIndex(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "lucide", "reference", "icons.md")
	if err := writeLucideIconIndex(filepath.Join(root, "lucide", "assets", "sprite.svg.gz"), dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"# Lucide " + lucide.Version + " icon names", "- `circle-plus`", "- `send`", "- `trash-2`"} {
		if !strings.Contains(got, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Index(got, "- `circle-plus`") > strings.Index(got, "- `send`") {
		t.Error("icon names are not sorted")
	}
}
