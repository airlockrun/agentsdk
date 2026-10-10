package mockairlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/provider/proxy"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/goai/tool"
)

func streamMock(t *testing.T, hook func(context.Context, *stream.CallOptions) (<-chan stream.Event, error)) *testutil.MockModel {
	t.Helper()
	model, err := testutil.NewMockModel(testutil.MockConfig{ID: t.Name(), Stream: hook})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func collect(t *testing.T, model stream.Model, ctx context.Context, opts *stream.CallOptions) []stream.Event {
	t.Helper()
	ch, err := model.Stream(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	var events []stream.Event
	for event := range ch {
		events = append(events, event)
	}
	return events
}

func TestModelsRouteAndForwardOptions(t *testing.T) {
	options := &stream.CallOptions{
		Messages: []message.Message{message.NewUserMessage("summarize this")},
		Tools: []tool.Tool{tool.New("lookup").Description("Lookup").SchemaFromStruct(struct {
			Query string `json:"query"`
		}{}).Build()},
		ToolChoice: "auto", ResponseFormat: &stream.ResponseFormat{Type: "json", Schema: json.RawMessage(`{"type":"object"}`)},
		Reasoning: stream.ReasoningEffortHigh, ProviderOptions: map[string]any{"test": true}, Headers: map[string]string{"X-Test": "value"},
	}
	seen := make(chan *stream.CallOptions, 2)
	makeModel := func(text string) stream.Model {
		return streamMock(t, func(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
			if opts.AbortSignal == nil || opts.AbortSignal.Err() != nil || ctx.Err() != nil {
				t.Error("model did not receive active request cancellation contexts")
			}
			seen <- opts
			ch := make(chan stream.Event, 4)
			for _, event := range testutil.MockTextResponse(text, testutil.MockUsage(7, 3)) {
				ch <- event
			}
			close(ch)
			return ch, nil
		})
	}
	m, url := NewWithModels(map[string]stream.Model{"": makeModel("default"), "summary": makeModel("named")})
	defer m.Close()
	for _, tc := range []struct{ slug, text string }{{"", "default"}, {"summary", "named"}} {
		t.Run(tc.text, func(t *testing.T) {
			model := proxy.Model("", proxy.Options{BaseURL: url, Slug: tc.slug, Capability: "text", Token: "test-token", Headers: map[string]string{"X-Airlock-Run-ID": "run-test"}})
			events := collect(t, model, t.Context(), options)
			want := testutil.MockTextResponse(tc.text, testutil.MockUsage(7, 3))
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %#v, want %#v", events, want)
			}
			got := <-seen
			got.AbortSignal = nil
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(options)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("options = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
	requests := m.Requests()
	if len(requests) != 2 || requests[0].Header.Get("Authorization") != "Bearer test-token" || requests[1].Header.Get("X-Airlock-Run-ID") != "run-test" {
		t.Fatalf("recorded requests = %+v", requests)
	}
}

func TestModelsForwardEventsAndErrors(t *testing.T) {
	errModel := errors.New("provider failed")
	for _, tc := range []struct {
		name   string
		events []stream.Event
		err    error
	}{
		{"reasoning", testutil.MockReasoningResponse("think", "answer", testutil.MockUsage(9, 4)), nil},
		{"tool call", testutil.MockToolCallResponse("call", "lookup", map[string]any{"query": "value"}, testutil.MockUsage(9, 4)), nil},
		{"tool result", []stream.Event{{Type: stream.EventToolResult, Data: stream.ToolResultEvent{ToolCallID: "call", ToolName: "lookup", Input: json.RawMessage(`{}`), Output: message.TextOutput{Value: "result"}}}}, nil},
		{"stream error", testutil.MockErrorResponse(errModel), nil},
		{"setup error", nil, errModel},
		{"nil stream", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := streamMock(t, func(context.Context, *stream.CallOptions) (<-chan stream.Event, error) {
				if tc.events == nil {
					return nil, tc.err
				}
				ch := make(chan stream.Event, len(tc.events))
				for _, event := range tc.events {
					ch <- event
				}
				close(ch)
				return ch, nil
			})
			m, url := NewWithModels(map[string]stream.Model{"summary": model})
			defer m.Close()
			got := collect(t, proxy.Model("", proxy.Options{BaseURL: url, Slug: "summary", Capability: "text"}), t.Context(), &stream.CallOptions{})
			if strings.Contains(tc.name, "error") || tc.name == "nil stream" {
				want := "provider failed"
				if tc.name == "nil stream" {
					want = "nil event stream"
				}
				if len(got) != 1 || got[0].Type != stream.EventError || !strings.Contains(got[0].Data.(stream.ErrorEvent).Error.Error(), want) {
					t.Fatalf("events = %+v", got)
				}
			} else if !reflect.DeepEqual(got, tc.events) {
				t.Fatalf("events = %#v, want %#v", got, tc.events)
			}
		})
	}
}

func TestModelsForwardAdditionalWireEvents(t *testing.T) {
	// Read the wire directly: the proxy client decides which event types it accepts.
	events := []stream.Event{
		{Type: stream.EventStart, Data: stream.StartEvent{Warnings: []stream.Warning{stream.OtherWarning("test warning")}}},
		{Type: stream.EventSource, Data: stream.SourceEvent{SourceType: stream.SourceTypeURL, ID: "source", URL: "https://example.com"}},
		{Type: stream.EventRawChunk, Data: stream.RawChunkEvent{RawValue: json.RawMessage(`{"value":42}`)}},
	}
	model := streamMock(t, func(context.Context, *stream.CallOptions) (<-chan stream.Event, error) {
		ch := make(chan stream.Event, len(events))
		for _, event := range events {
			ch <- event
		}
		close(ch)
		return ch, nil
	})
	m, url := NewWithModels(map[string]stream.Model{"": model})
	defer m.Close()
	resp, err := http.Post(url+"/api/agent/llm/stream", "application/json", strings.NewReader(`{"capability":"text","options":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for _, event := range events {
		var got struct {
			Type stream.EventType `json:"type"`
			Data json.RawMessage  `json:"data"`
		}
		if err := decoder.Decode(&got); err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != event.Type || !bytes.Equal(got.Data, want) {
			t.Fatalf("event = %s %s, want %s %s", got.Type, got.Data, event.Type, want)
		}
	}
}

func TestModelsErrorPayloadRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload stream.EventData
		want    string
	}{
		{"value", stream.ErrorEvent{Error: errors.New("provider value failure")}, "provider value failure"},
		{"pointer", &stream.ErrorEvent{Error: errors.New("provider pointer failure")}, "provider pointer failure"},
		{"nil value error", stream.ErrorEvent{}, "agentsdk: invalid stream error event: nil error"},
		{"nil pointer error", &stream.ErrorEvent{}, "agentsdk: invalid stream error event: nil error"},
		{"nil pointer payload", (*stream.ErrorEvent)(nil), "agentsdk: invalid stream error event: nil payload"},
		{"missing payload", nil, "agentsdk: invalid stream error event: missing or invalid payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := stream.Event{Type: stream.EventError, Data: tc.payload}
			config := testutil.MockConfig{ID: t.Name()}
			if tc.name == "nil pointer payload" {
				// The canonical mock's custom stream hook can exercise malformed
				// provider events rejected by its declarative response validation.
				config.Stream = func(context.Context, *stream.CallOptions) (<-chan stream.Event, error) {
					ch := make(chan stream.Event, 1)
					ch <- event
					close(ch)
					return ch, nil
				}
			} else {
				config.Default = &testutil.MockResponse{Events: []stream.Event{event}}
			}
			model, err := testutil.NewMockModel(config)
			if err != nil {
				t.Fatal(err)
			}
			m, url := NewWithModels(map[string]stream.Model{"summary": model})
			defer m.Close()
			client := proxy.Model("", proxy.Options{BaseURL: url, Slug: "summary", Capability: "text"})
			events := collect(t, client, t.Context(), &stream.CallOptions{})
			if len(events) != 1 || events[0].Type != stream.EventError {
				t.Fatalf("events = %+v", events)
			}
			payload, ok := events[0].Data.(stream.ErrorEvent)
			if !ok || payload.Error == nil {
				t.Fatalf("missing proxy error: %+v", events[0])
			}
			if got := payload.Error.Error(); got != tc.want {
				t.Fatalf("proxy error = %q, want %q", got, tc.want)
			}
			if len(model.Requests()) != 1 || len(m.RequestsByPath("/api/agent/llm/stream")) != 1 {
				t.Fatal("error did not traverse model and HTTP proxy")
			}
		})
	}
}

func TestModelsRejectUnboundAndInvalidRequests(t *testing.T) {
	model := streamMock(t, func(context.Context, *stream.CallOptions) (<-chan stream.Event, error) {
		t.Error("unexpected model call")
		return nil, errors.New("unexpected call")
	})
	m, url := NewWithModels(map[string]stream.Model{"summary": model})
	defer m.Close()
	for _, body := range []string{
		`{"slug":"missing","capability":"text","options":{}}`,
		`{"capability":"text","options":{}}`,
		`{"slug":"summary","capability":"image","options":{}}`,
		`{"capability":"vision","options":{}}`,
		`{"slug":"summary","capability":"text","options":null}`,
		`{"slug":"summary","capability":"text"}`,
		`{"slug":"summary","capability":"text","options":{}} {}`,
		`invalid`,
	} {
		t.Run(body, func(t *testing.T) {
			resp, err := http.Post(url+"/api/agent/llm/stream", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		})
	}
}

func TestModelsCancellationAndIncrementalFlush(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	model := streamMock(t, func(ctx context.Context, _ *stream.CallOptions) (<-chan stream.Event, error) {
		ch := make(chan stream.Event)
		go func() {
			defer close(ch)
			close(started)
			select {
			case ch <- stream.Event{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "first"}}:
			case <-ctx.Done():
			}
			<-ctx.Done()
			close(canceled)
		}()
		return ch, nil
	})
	m, url := NewWithModels(map[string]stream.Model{"": model})
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/agent/llm/stream", strings.NewReader(`{"capability":"text","options":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	var line struct {
		Type string                `json:"type"`
		Data stream.TextDeltaEvent `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&line); err != nil {
		t.Fatal(err)
	}
	if line.Type != "text-delta" || line.Data.Text != "first" {
		t.Fatalf("first event = %+v", line)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("model request context was not canceled")
	}
}

func TestModelsNoCrossSlotFallback(t *testing.T) {
	model := streamMock(t, func(context.Context, *stream.CallOptions) (<-chan stream.Event, error) {
		t.Error("unbound call reached another slot's model")
		return nil, errors.New("unexpected call")
	})
	for _, tc := range []struct {
		name     string
		bindings map[string]stream.Model
		slug     string
	}{
		{"default does not bind named slots", map[string]stream.Model{"": model}, "missing"},
		{"named slot does not bind default", map[string]stream.Model{"summary": model}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, url := NewWithModels(tc.bindings)
			defer m.Close()
			events := collect(t, proxy.Model("", proxy.Options{BaseURL: url, Slug: tc.slug, Capability: "text"}), t.Context(), &stream.CallOptions{})
			if len(events) != 1 || events[0].Type != stream.EventError {
				t.Fatalf("events = %+v", events)
			}
			if err := events[0].Data.(stream.ErrorEvent).Error; !strings.Contains(err.Error(), "request failed") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDefaultCanonicalModel(t *testing.T) {
	m, url := New()
	defer m.Close()
	got := collect(t, proxy.Model("", proxy.Options{BaseURL: url, Capability: "text"}), t.Context(), &stream.CallOptions{})
	if len(got) != 5 || got[2].Data.(stream.TextDeltaEvent).Text != "Hello" {
		t.Fatalf("events = %+v", got)
	}
}
