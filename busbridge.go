package agentsdk

import (
	"encoding/json"

	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
)

// streamBusToNDJSON subscribes to all bus events and forwards them to the
// EventWriter as NDJSON. Closes done when the bus unsubscribes (call the
// returned unsub function to trigger this).
func streamBusToNDJSON(b *bus.Bus, ew *EventWriter) func() {
	// bus.PermissionAsked fires twice in the in-process case: once from
	// PermissionManager.Ask and again from the runner's step-complete
	// re-publish (intended for the toolserver case where the two buses
	// are separate). Dedupe by toolCallID so bridges render a single
	// confirmation dialog regardless of execution mode.
	seenPerm := make(map[string]struct{})
	return b.SubscribeAll(func(e bus.Event) {
		switch e.Type {
		case bus.StreamTextDelta:
			if delta, ok := e.Properties.(stream.TextDeltaEvent); ok {
				ew.WriteEvent(stream.Event{Type: stream.EventTextDelta, Data: delta})
			}
		case bus.StreamToolCall:
			if tc, ok := e.Properties.(stream.ToolCallEvent); ok {
				ew.WriteEvent(stream.Event{Type: stream.EventToolCall, Data: tc})
			}
		case bus.StreamToolResult:
			if tr, ok := e.Properties.(stream.ToolResultEvent); ok {
				ew.WriteEvent(stream.Event{Type: stream.EventToolResult, Data: tr})
			}
		case bus.PermissionAsked:
			if payload, ok := e.Properties.(bus.PermissionAskedPayload); ok {
				if payload.ToolCallID != "" {
					if _, dup := seenPerm[payload.ToolCallID]; dup {
						return
					}
					seenPerm[payload.ToolCallID] = struct{}{}
				}
				ew.writeLine(ndjsonLine{
					Type: "confirmation_required",
					Data: map[string]any{
						"permission": payload.Permission,
						"patterns":   payload.Patterns,
						"code":       payload.Metadata["code"],
						"toolCallId": payload.ToolCallID,
					},
				})
			}
		}
	})
}

// emitSuspensionEvent writes the suspension context as an NDJSON event.
func emitSuspensionEvent(ew *EventWriter, sc *sol.SuspensionContext) {
	if sc == nil {
		return
	}
	data, _ := json.Marshal(sc)
	var m map[string]any
	json.Unmarshal(data, &m)
	ew.writeLine(ndjsonLine{
		Type: "suspended",
		Data: m,
	})
}
