package agentsdk

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestBundledHTMX(t *testing.T) {
	data, err := bundledAssets.ReadFile("assets/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprintf("%x", sha256.Sum256(data)), "e484d9171a9db30a39c8f16e3d709d4137f3211c659f8e6125816635033d593f"; got != want {
		t.Fatalf("htmx asset SHA-256 = %s, want %s", got, want)
	}
	if got, want := Assets.HTMX, "/__air/assets/htmx-4.0.0.min.js"; got != want {
		t.Fatalf("Assets.HTMX = %q, want %q", got, want)
	}
}
