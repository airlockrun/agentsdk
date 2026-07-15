package agentsdk

import (
	"fmt"
	"testing"
)

func TestNewRequiresDatabaseURL(t *testing.T) {
	t.Setenv("AIRLOCK_AGENT_ID", "test-agent")
	t.Setenv("AIRLOCK_API_URL", "http://127.0.0.1")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "test-token")
	t.Setenv("AIRLOCK_DB_URL", "")

	defer func() {
		got := fmt.Sprint(recover())
		want := "agentsdk: required environment variable AIRLOCK_DB_URL is not set"
		if got != want {
			t.Fatalf("panic = %q, want %q", got, want)
		}
	}()
	New(Config{Description: "test"})
}
