// Package streamevent encodes GoAI events for SDK NDJSON streams.
package streamevent

import "github.com/airlockrun/goai/stream"

// Line is one NDJSON event envelope.
type Line struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// FromEvent converts value and pointer error payloads to their wire
// representation. Malformed error payloads produce a descriptive error event.
func FromEvent(event stream.Event) Line {
	switch data := event.Data.(type) {
	case stream.ErrorEvent:
		return errorLine(data.Error)
	case *stream.ErrorEvent:
		if data == nil {
			return errorMessage("agentsdk: invalid stream error event: nil payload")
		}
		return errorLine(data.Error)
	default:
		if event.Type == stream.EventError {
			return errorMessage("agentsdk: invalid stream error event: missing or invalid payload")
		}
		return Line{Type: string(event.Type), Data: event.Data}
	}
}

func errorLine(err error) Line {
	if err == nil {
		return errorMessage("agentsdk: invalid stream error event: nil error")
	}
	return errorMessage(err.Error())
}

func errorMessage(message string) Line {
	return Line{Type: string(stream.EventError), Data: struct {
		Error string `json:"error"`
	}{message}}
}
