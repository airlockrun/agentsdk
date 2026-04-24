package agentsdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/airlockrun/goai/stream"
)

// EventWriter streams NDJSON events to an HTTP response.
type EventWriter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	headersSent bool
	mu          sync.Mutex
}

func newEventWriter(w http.ResponseWriter) *EventWriter {
	flusher, ok := w.(http.Flusher)
	if !ok {
		panic("agentsdk: ResponseWriter does not implement http.Flusher")
	}
	return &EventWriter{w: w, flusher: flusher}
}

// ndjsonLine is the wire format for a single NDJSON event.
type ndjsonLine struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

func (ew *EventWriter) ensureHeaders() {
	if !ew.headersSent {
		ew.w.Header().Set("Content-Type", "application/x-ndjson")
		ew.w.Header().Set("Transfer-Encoding", "chunked")
		ew.headersSent = true
	}
}

func (ew *EventWriter) writeLine(line ndjsonLine) error {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	ew.ensureHeaders()
	b, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("agentsdk: marshal event: %w", err)
	}
	b = append(b, '\n')
	if _, err := ew.w.Write(b); err != nil {
		return err
	}
	ew.flusher.Flush()
	return nil
}

// WriteEvent serializes a GoAI stream.Event as an NDJSON line.
func (ew *EventWriter) WriteEvent(event stream.Event) error {
	data := marshalEventData(event.Data)
	return ew.writeLine(ndjsonLine{
		Type: string(event.Type),
		Data: data,
	})
}

// WriteProgress writes a progress event (for webhook/cron handlers).
func (ew *EventWriter) WriteProgress(message string) error {
	return ew.writeLine(ndjsonLine{
		Type: "progress",
		Data: map[string]string{"message": message},
	})
}

// WriteError writes an error event.
func (ew *EventWriter) WriteError(err error) error {
	return ew.writeLine(ndjsonLine{
		Type: string(stream.EventError),
		Data: map[string]string{"error": err.Error()},
	})
}

// marshalEventData converts EventData to a JSON-safe representation.
// Handles error interfaces that don't marshal cleanly.
func marshalEventData(data stream.EventData) any {
	switch d := data.(type) {
	case stream.ErrorEvent:
		return struct {
			Error string `json:"error"`
		}{d.Error.Error()}
	case stream.ToolErrorEvent:
		return struct {
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Input      json.RawMessage `json:"input,omitempty"`
			Error      string          `json:"error"`
		}{d.ToolCallID, d.ToolName, d.Input, d.Error.Error()}
	default:
		return data
	}
}
