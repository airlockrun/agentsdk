package agentsdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/airlockrun/agentsdk/internal/streamevent"
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
type ndjsonLine = streamevent.Line

func (ew *EventWriter) ensureHeaders() {
	if !ew.headersSent {
		ew.w.Header().Set("Content-Type", "application/x-ndjson")
		ew.w.Header().Set("Transfer-Encoding", "chunked")
		ew.headersSent = true
	}
}

// flushHeaders commits the streaming response before the first event is
// available. This lets the caller retain control of a run while the model is
// still waiting to produce output.
func (ew *EventWriter) flushHeaders() {
	ew.mu.Lock()
	defer ew.mu.Unlock()
	ew.ensureHeaders()
	ew.flusher.Flush()
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
	return ew.writeLine(streamevent.FromEvent(event))
}

// WriteProgress writes a progress event for webhook handlers.
func (ew *EventWriter) WriteProgress(message string) error {
	return ew.writeLine(ndjsonLine{
		Type: "progress",
		Data: map[string]string{"message": message},
	})
}

// WriteError writes an error event.
func (ew *EventWriter) WriteError(err error) error {
	return ew.WriteEvent(stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{Error: err}})
}
