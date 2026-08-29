//go:build darwin

package connector

import (
	"path/filepath"
	"testing"
)

func TestDarwinDefaultStateDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	user, err := defaultStateDir("sample", ServiceUser)
	if err != nil {
		t.Fatal(err)
	}
	wantUser := filepath.Join(home, "Library", "Application Support", "Airlock", "Connectors", "sample")
	if user != wantUser {
		t.Fatalf("user state directory = %q, want %q", user, wantUser)
	}
	if _, err := defaultStateDir("sample", ServiceSystem); err == nil {
		t.Fatal("defaultStateDir accepted a macOS system service")
	}
}
