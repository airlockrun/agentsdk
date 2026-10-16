package jsexec

import "testing"

func TestRuntimeFingerprint(t *testing.T) {
	if RuntimeFingerprint() != publishedCompatibleRuntime {
		t.Fatal("embedded runtime differs from reviewed artifact; validate compatibility before updating fingerprint")
	}
}
