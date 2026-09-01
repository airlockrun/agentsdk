package connector

import (
	"fmt"
	"strings"
	"testing"
)

func assertPanicContains(t *testing.T, expected string, run func()) {
	t.Helper()
	defer func() {
		message := fmt.Sprint(recover())
		if !strings.Contains(message, expected) {
			t.Fatalf("panic = %q, want text %q", message, expected)
		}
	}()
	run()
}
