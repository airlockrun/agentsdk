package agentsdk

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/sol/bus"
)

func TestNDJSONSinkAutomaticCompactionLifecycle(t *testing.T) {
	recorder := httptest.NewRecorder()
	sink := newNDJSONSink(newEventWriter(recorder))

	sink.OnAutomaticCompactionStarted(bus.AutomaticCompactionStartedPayload{})
	sink.OnAutomaticCompactionFinished(bus.AutomaticCompactionFinishedPayload{
		TokensFreed: 17,
		Error:       "model failed",
	})

	lines := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %s", len(lines), recorder.Body.String())
	}
	var events []struct {
		Type string `json:"type"`
		Data struct {
			TokensFreed int    `json:"tokensFreed"`
			Error       string `json:"error"`
		} `json:"data"`
	}
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
			Data struct {
				TokensFreed int    `json:"tokensFreed"`
				Error       string `json:"error"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		events = append(events, event)
	}

	if events[0].Type != "compaction_started" {
		t.Errorf("started type = %q", events[0].Type)
	}
	if events[1].Type != "compaction_finished" || events[1].Data.TokensFreed != 17 || events[1].Data.Error != "model failed" {
		t.Errorf("finished event = %+v", events[1])
	}
}
