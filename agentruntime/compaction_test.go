package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/session"
)

// Token accounting is a host model concern. This scripted wrapper also observes
// compaction requests, which must never bypass BeforeModel or usage accounting.
type meteredModel struct {
	stream.Model
	controller *controller
	tokens     int
	requests   int
}

func (m *meteredModel) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	if m.controller.steps != m.requests+1 {
		return nil, errors.New("request without budget reservation")
	}
	m.requests++
	events, err := m.Model.Stream(ctx, opts)
	if err != nil {
		return nil, err
	}
	var saved []stream.Event
	for e := range events {
		if finish, ok := e.Data.(stream.FinishEvent); ok {
			m.tokens += finish.Usage.InputTotal() + finish.Usage.OutputTotal()
		}
		saved = append(saved, e)
	}
	out := make(chan stream.Event, len(saved))
	for _, e := range saved {
		out <- e
	}
	close(out)
	return out, nil
}

func TestCompactionUsesSessionPersistenceAndBudgetedModel(t *testing.T) {
	in, model := input(testutil.MockTextResponse("Summary of completed work.", testutil.MockUsage(200, 30)), batch(complete("done")))
	store := in.Store.(*memoryStore)
	store.messages = []session.Message{{ID: "old", Role: "user", Content: strings.Repeat("old secret context ", 2000)}}
	in.Redactor = func(s string) string { return strings.ReplaceAll(s, "secret", "[redacted]") }
	in.ModelLimits = session.ModelLimits{Input: 5000, Output: 1000}
	meter := &meteredModel{Model: model, controller: in.Controller.(*controller)}
	in.Model = meter
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || store.compactions != 1 || meter.requests != 2 || meter.tokens != 250 {
		t.Fatalf("result=%+v err=%v compactions=%d meter=%+v", result, err, store.compactions, meter)
	}
	if len(model.DoStreamCalls[0].Tools) != 0 || len(model.DoStreamCalls[1].Tools) == 0 {
		t.Fatal("compaction and reasoning tool sets are wrong")
	}
	raw, _ := json.Marshal(model.DoStreamCalls)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("compaction leaked an unredacted secret")
	}
	if strings.Contains(string(raw), "old secret context") {
		t.Fatal("uncompacted transcript used")
	}
	if len(in.Sink.(*sink).compactions) != 1 || in.Sink.(*sink).compactions[0].TokensFreed <= 0 {
		t.Fatal("missing compaction event")
	}
	foundSummary := false
	for _, m := range store.messages {
		foundSummary = foundSummary || m.Summary
	}
	if !foundSummary {
		t.Fatal("summary not persisted")
	}
}

func TestCompactionFailureAndBudgetDenialPreserveHistory(t *testing.T) {
	for _, mode := range []string{"budget", "stream", "oversized summary"} {
		t.Run(mode, func(t *testing.T) {
			response := []stream.Event{{Type: stream.EventError, Data: stream.ErrorEvent{Error: errCrash}}}
			if mode == "oversized summary" {
				response = testutil.MockTextResponse(strings.Repeat("long", 10000), testutil.MockUsage(100, 100))
			}
			in, model := input(response)
			store := in.Store.(*memoryStore)
			store.messages = []session.Message{{ID: "old", Role: "user", Content: strings.Repeat("context", 10000)}}
			in.ModelLimits = session.ModelLimits{Input: 5000, Output: 1000}
			if mode == "budget" {
				in.Controller.(*controller).beforeErr = errors.New("budget exhausted")
			}
			if _, err := Run(t.Context(), in); err == nil {
				t.Fatal("expected compaction failure")
			}
			if mode == "budget" && len(model.DoStreamCalls) != 0 {
				t.Fatal("budget-denied model request")
			}
			if mode != "oversized summary" && (store.compactions != 0 || store.messages[0].ID != "old") {
				t.Fatal("failed compaction changed history")
			}
			if len(in.Sink.(*sink).compactions) != 1 || in.Sink.(*sink).compactions[0].Error == "" {
				t.Fatal("missing compaction failure")
			}
		})
	}
}

func TestCompactionNeverRunsInsideResumedBatch(t *testing.T) {
	in, model := input(batch(call("wait", "wait_calls", `{"ids":["child"],"mode":"any"}`), complete("done")))
	c := in.Controller.(*controller)
	c.waiting = true
	if _, err := Run(t.Context(), in); !errors.Is(err, ErrWaiting) {
		t.Fatal(err)
	}
	store := in.Store.(*memoryStore)
	store.messages[0].Content = strings.Repeat("huge context", 100000)
	c.waiting = false
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || store.compactions != 0 || len(model.DoStreamCalls) != 1 {
		t.Fatalf("result=%+v err=%v compactions=%d requests=%d", result, err, store.compactions, len(model.DoStreamCalls))
	}
}

func TestCompactionPrunesWithoutSummaryModel(t *testing.T) {
	in, model := input(batch(complete("done")))
	store := in.Store.(*memoryStore)
	store.messages = []session.Message{
		{ID: "old", Role: "user", Content: "old work"},
		{ID: "large", Role: "tool", Parts: []session.Part{{Type: "tool", Tool: &session.ToolPart{CallID: "old-call", Name: "run_js", Status: "completed", Result: true, OutputType: "text", Output: strings.Repeat("data", 80000)}}}},
		{ID: "middle", Role: "user", Content: "middle work"},
		{ID: "recent", Role: "user", Content: "recent work"},
	}
	in.ModelLimits = session.ModelLimits{Input: 20000, Output: 1000}
	result, err := Run(t.Context(), in)
	if err != nil || result.Reply == nil || store.compactions != 1 || len(model.DoStreamCalls) != 1 || in.Controller.(*controller).steps != 1 {
		t.Fatalf("result=%+v error=%v compactions=%d requests=%d", result, err, store.compactions, len(model.DoStreamCalls))
	}
	if !store.messages[1].Parts[0].Tool.Compacted {
		t.Fatal("old tool output was not pruned")
	}
}
