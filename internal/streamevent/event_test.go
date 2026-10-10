package streamevent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/airlockrun/goai/stream"
)

func TestFromEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event stream.Event
		want  string
	}{
		{"text", stream.Event{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "hello"}}, `{"type":"text-delta","data":{"text":"hello"}}`},
		{"error", stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{Error: errors.New("failed")}}, `{"type":"error","data":{"error":"failed"}}`},
		{"pointer error", stream.Event{Type: stream.EventError, Data: &stream.ErrorEvent{Error: errors.New("pointer failed")}}, `{"type":"error","data":{"error":"pointer failed"}}`},
		{"nil value error", stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{}}, `{"type":"error","data":{"error":"agentsdk: invalid stream error event: nil error"}}`},
		{"nil pointer error", stream.Event{Type: stream.EventError, Data: &stream.ErrorEvent{}}, `{"type":"error","data":{"error":"agentsdk: invalid stream error event: nil error"}}`},
		{"nil pointer payload", stream.Event{Type: stream.EventError, Data: (*stream.ErrorEvent)(nil)}, `{"type":"error","data":{"error":"agentsdk: invalid stream error event: nil payload"}}`},
		{"missing payload", stream.Event{Type: stream.EventError}, `{"type":"error","data":{"error":"agentsdk: invalid stream error event: missing or invalid payload"}}`},
		{"wrong payload", stream.Event{Type: stream.EventError, Data: stream.TextDeltaEvent{Text: "not an error"}}, `{"type":"error","data":{"error":"agentsdk: invalid stream error event: missing or invalid payload"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(FromEvent(tc.event))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("JSON = %s, want %s", got, tc.want)
			}
		})
	}
}
