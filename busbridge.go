package agentsdk

import (
	"encoding/json"

	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/eventstream"
)

// ndjsonSink implements eventstream.Sink by writing each event as a
// line into an EventWriter. The translation is the agent-side wire
// shape airlock parses (see airlock/api/event_publisher.go); keep the
// event names + payload shapes here in lockstep with that parser.
//
// PermissionAsked dedupe: in the in-process case PermissionManager.Ask
// fires the event AND the runner's step-complete re-publishes it
// (intended for the toolserver case where the two buses are
// separate). Dedupe by toolCallID so bridges render a single
// confirmation dialog regardless of execution mode.
type ndjsonSink struct {
	ew       *EventWriter
	seenPerm map[string]struct{}
}

func newNDJSONSink(ew *EventWriter) *ndjsonSink {
	return &ndjsonSink{ew: ew, seenPerm: make(map[string]struct{})}
}

func (s *ndjsonSink) OnTextDelta(e stream.TextDeltaEvent) {
	_ = s.ew.WriteEvent(stream.Event{Type: stream.EventTextDelta, Data: e})
}

func (s *ndjsonSink) OnToolCall(e stream.ToolCallEvent) {
	_ = s.ew.WriteEvent(stream.Event{Type: stream.EventToolCall, Data: e})
}

func (s *ndjsonSink) OnToolResult(e stream.ToolResultEvent) {
	_ = s.ew.WriteEvent(stream.ToolOutcomeEvent(e.ToolCallID, e.ToolName, e.Input, e.Output))
}

func (s *ndjsonSink) OnPermissionAsked(p bus.PermissionAskedPayload) {
	if p.ToolCallID != "" {
		if _, dup := s.seenPerm[p.ToolCallID]; dup {
			return
		}
		s.seenPerm[p.ToolCallID] = struct{}{}
	}
	_ = s.ew.writeLine(ndjsonLine{
		Type: "confirmation_required",
		Data: map[string]any{
			"permission": p.Permission,
			"patterns":   p.Patterns,
			"code":       p.Metadata["code"],
			"toolCallId": p.ToolCallID,
		},
	})
}

// OnSuspension serializes the suspension snapshot for the resume
// path and, if the suspension is delegated (A2A child gate),
// synthesizes the leaf confirmation_required so the existing approval
// pipeline drives it end-to-end without a separate UI path.
func (s *ndjsonSink) OnSuspension(sc *sol.SuspensionContext) {
	if sc == nil {
		return
	}
	data, _ := json.Marshal(sc)
	var m map[string]any
	json.Unmarshal(data, &m)
	_ = s.ew.writeLine(ndjsonLine{Type: "suspended", Data: m})

	// A delegated suspension carries no local PermissionAsked, so no
	// confirmation_required was emitted by the bus bridge. Synthesize
	// one from the carried leaf gate detail so the EXISTING confirm
	// pipeline (airlock → frontend card → approve/deny → resume with
	// Approved → resolveDelegatedSuspension) drives it end to end —
	// the down-cascade. Attribution rides in permission/code so the
	// human sees which sibling wants to do what.
	if sc.Reason == "delegated" {
		var del struct {
			ToolCallID string `json:"toolCallID"`
			Child      struct {
				Slug         string `json:"slug"`
				Confirmation struct {
					Agent      string   `json:"agent"`
					Permission string   `json:"permission"`
					Patterns   []string `json:"patterns"`
					Code       string   `json:"code"`
				} `json:"confirmation"`
			} `json:"child"`
		}
		raw, _ := json.Marshal(sc.Data)
		_ = json.Unmarshal(raw, &del)
		who := del.Child.Confirmation.Agent
		if who == "" {
			who = del.Child.Slug
		}
		perm := "promptAgent"
		if del.Child.Confirmation.Permission != "" {
			perm = del.Child.Confirmation.Permission
		}
		_ = s.ew.writeLine(ndjsonLine{
			Type: "confirmation_required",
			Data: map[string]any{
				"permission": who + ": " + perm,
				"patterns":   del.Child.Confirmation.Patterns,
				"code":       del.Child.Confirmation.Code,
				"toolCallId": del.ToolCallID,
			},
		})
	}
}

// streamBusToNDJSON subscribes an NDJSON sink to b for the lifetime
// of a run. Returns the unsubscribe func.
func streamBusToNDJSON(b *bus.Bus, ew *EventWriter) func() {
	return eventstream.Forward(b, newNDJSONSink(ew))
}

// emitSuspensionEvent writes the suspension context as an NDJSON
// event. Out-of-band relative to the bus (suspension rides on
// RunResult, not on a bus event), so it's a direct sink call.
func emitSuspensionEvent(ew *EventWriter, sc *sol.SuspensionContext) {
	newNDJSONSink(ew).OnSuspension(sc)
}
