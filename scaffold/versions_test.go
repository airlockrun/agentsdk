package scaffold

import (
	"go/version"
	"os"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestGoVersionSupportsSDK(t *testing.T) {
	data, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m, err := modfile.Parse("../go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Go == nil {
		t.Fatal("SDK go.mod has no Go version")
	}
	if version.Compare("go"+GoVersion, "go"+m.Go.Version) < 0 {
		t.Fatalf("scaffold Go %s cannot build SDK requiring Go %s", GoVersion, m.Go.Version)
	}
}
