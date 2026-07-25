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
	_ = s.ew.WriteEvent(stream.ToolOutcomeEvent(e.ToolCallID, e.ToolName, e.Input, e.Output, e.Title, e.Metadata))
}

func (s *ndjsonSink) OnPermissionAsked(p bus.PermissionAskedPayload) {
	if p.ToolCallID != "" {
		if _, dup := s.seenPerm[p.ToolCallID]; dup {
			return
		}
		s.seenPerm[p.ToolCallID] = struct{}{}
	}
	// "code" is kept as a top-level field so older airlock versions
	// (pre-metadata) still render run_js confirmations. "metadata" carries
	// the full payload so newer airlock can pick the right body for
	// non-run_js permissions (args for sysagent-style tools, message for
	// doom_loop, etc.).
	_ = s.ew.writeLine(ndjsonLine{
		Type: "confirmation_required",
		Data: map[string]any{
			"permission": p.Permission,
			"patterns":   p.Patterns,
			"code":       p.Metadata["code"],
			"metadata":   p.Metadata,
			"toolCallId": p.ToolCallID,
		},
	})
}

func (s *ndjsonSink) OnAutomaticCompactionStarted(p bus.AutomaticCompactionStartedPayload) {
	_ = s.ew.writeLine(ndjsonLine{Type: "compaction_started", Data: p})
}

func (s *ndjsonSink) OnAutomaticCompactionFinished(p bus.AutomaticCompactionFinishedPayload) {
	_ = s.ew.writeLine(ndjsonLine{Type: "compaction_finished", Data: p})
}

// OnSuspension serializes the suspension snapshot for the resume
// path and, if the suspension is delegated,
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
		toolCallID, confirmation := delegatedConfirmation(sc.Data)
		perm := "promptAgent"
		if confirmation.Permission != "" {
			perm = confirmation.Permission
		}
		if confirmation.Agent != "" {
			perm = confirmation.Agent + ": " + perm
		}
		_ = s.ew.writeLine(ndjsonLine{
			Type: "confirmation_required",
			Data: map[string]any{
				"permission": perm,
				"patterns":   confirmation.Patterns,
				"code":       confirmation.Code,
				"metadata":   confirmation.Metadata,
				"toolCallId": toolCallID,
			},
		})
	}
}

type suspensionConfirmation struct {
	Agent      string
	Permission string
	Patterns   []string
	Code       string
	Metadata   map[string]any
}

func delegatedConfirmation(data any) (string, suspensionConfirmation) {
	raw, _ := json.Marshal(data)
	var delegated struct {
		ToolCallID string          `json:"toolCallID"`
		Transport  string          `json:"transport"`
		Child      json.RawMessage `json:"child"`
	}
	_ = json.Unmarshal(raw, &delegated)

	switch delegated.Transport {
	case "a2a":
		var child struct {
			Slug         string                 `json:"slug"`
			Confirmation suspensionConfirmation `json:"confirmation"`
		}
		_ = json.Unmarshal(delegated.Child, &child)
		if child.Confirmation.Agent == "" {
			child.Confirmation.Agent = child.Slug
		}
		return delegated.ToolCallID, child.Confirmation
	case "inprocess":
		var child struct {
			AgentName         string `json:"agentName"`
			SuspensionContext struct {
				Reason string          `json:"reason"`
				Data   json.RawMessage `json:"data"`
			} `json:"suspensionContext"`
		}
		_ = json.Unmarshal(delegated.Child, &child)
		var confirmation suspensionConfirmation
		switch child.SuspensionContext.Reason {
		case "permission":
			_ = json.Unmarshal(child.SuspensionContext.Data, &confirmation)
			if code, ok := confirmation.Metadata["code"].(string); ok {
				confirmation.Code = code
			}
		case "delegated":
			_, confirmation = delegatedConfirmation(child.SuspensionContext.Data)
		}
		if confirmation.Agent == "" {
			confirmation.Agent = child.AgentName
		}
		return delegated.ToolCallID, confirmation
	default:
		return delegated.ToolCallID, suspensionConfirmation{}
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
