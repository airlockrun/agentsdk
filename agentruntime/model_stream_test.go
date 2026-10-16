package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/jsexec"
	goaierrors "github.com/airlockrun/goai/errors"
	"github.com/airlockrun/goai/provider/openai"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/testutil"
	"golang.org/x/net/http2"
)

func transientStreamError() error {
	return goaierrors.NewAPICallError(goaierrors.APICallErrorOptions{
		Message: "stream read failed", Cause: http2.StreamError{StreamID: 187, Code: http2.ErrCodeInternal},
		IsRetryable: true, IsRetryableSet: true,
	})
}

func errorEvent(err error) stream.Event {
	return stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{Error: err}}
}

func captureForTest(model stream.Model, c *controller, failures *[]ModelStreamFailure) *modelCapture {
	return &modelCapture{Model: model, before: c.BeforeModel,
		failed: func(_ context.Context, failure ModelStreamFailure) error {
			*failures = append(*failures, failure)
			return nil
		}, backoff: func(int) time.Duration { return 0 },
	}
}

func TestModelCaptureRecovery(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix []stream.Event
		known  bool
	}{
		{name: "before first event"},
		{name: "partial text", prefix: []stream.Event{{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "discard me"}}}},
		{name: "partial tool JSON", prefix: []stream.Event{
			{Type: stream.EventToolInputStart, Data: stream.ToolInputStartEvent{ID: "unfinished", ToolName: "run_js"}},
			{Type: stream.EventToolInputDelta, Data: stream.ToolInputDeltaEvent{ID: "unfinished", Delta: `{"code":"`}},
		}},
		{name: "complete uncommitted call", prefix: []stream.Event{js("discarded")}},
		{name: "after usage", prefix: batch(js("discarded")), known: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			failed := append(append([]stream.Event(nil), tt.prefix...), errorEvent(transientStreamError()))
			_, model := input(failed, batch(complete("done")))
			c := &controller{}
			var failures []ModelStreamFailure
			m := captureForTest(model, c, &failures)
			opts := &stream.CallOptions{}
			events, err := m.Stream(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			var got []stream.Event
			for event := range events {
				got = append(got, event)
			}
			if !reflect.DeepEqual(got, batch(complete("done"))) {
				t.Fatalf("events=%+v", got)
			}
			if c.steps != 2 || len(failures) != 1 || failures[0].UsageUnknown == tt.known {
				t.Fatalf("steps=%d failures=%+v", c.steps, failures)
			}
			if tt.known && failures[0].Usage.GrandTotal() != 20 {
				t.Fatal(failures)
			}
			requests := model.Requests()
			if len(requests) != 2 || !reflect.DeepEqual(requests[0].Messages, requests[1].Messages) {
				t.Fatal(requests)
			}
		})
	}
}

func TestModelCaptureTerminalErrorsAndBudgets(t *testing.T) {
	for _, tt := range []struct {
		name     string
		events   []stream.Event
		limit    int
		attempts int
	}{
		{"exhaustion", []stream.Event{errorEvent(transientStreamError())}, 0, 3},
		{"step budget", []stream.Event{errorEvent(transientStreamError())}, 1, 1},
		{"auth", []stream.Event{errorEvent(goaierrors.NewAPICallError(goaierrors.APICallErrorOptions{StatusCode: 401}))}, 0, 1},
		{"bad request even if mislabeled retryable", []stream.Event{errorEvent(goaierrors.NewAPICallError(goaierrors.APICallErrorOptions{StatusCode: 400, IsRetryable: true, IsRetryableSet: true}))}, 0, 1},
		{"schema", []stream.Event{errorEvent(errors.New("invalid provider options"))}, 0, 1},
		{"context error", []stream.Event{errorEvent(context.Canceled)}, 0, 1},
		{"missing finish", []stream.Event{{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "truncated"}}}, 0, 1},
		{"empty stream", []stream.Event{}, 0, 1},
		{"provider effect", []stream.Event{{Type: stream.EventToolInputStart, Data: stream.ToolInputStartEvent{ID: "external", ToolName: "external", ProviderExecuted: true}}, errorEvent(transientStreamError())}, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, model := input(tt.events, tt.events, tt.events, batch(complete("never")))
			c := &controller{limit: tt.limit}
			var failures []ModelStreamFailure
			m := captureForTest(model, c, &failures)
			events, err := m.Stream(t.Context(), &stream.CallOptions{})
			if err == nil || events != nil || len(model.Requests()) != tt.attempts {
				t.Fatalf("events=%v err=%v attempts=%d", events, err, len(model.Requests()))
			}
		})
	}
}

func TestModelCaptureCancellation(t *testing.T) {
	for _, phase := range []string{"before request", "during backoff", "deadline", "abort signal"} {
		t.Run(phase, func(t *testing.T) {
			_, model := input([]stream.Event{errorEvent(transientStreamError())}, batch(complete("never")))
			c := &controller{}
			var failures []ModelStreamFailure
			m := captureForTest(model, c, &failures)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := &stream.CallOptions{}
			wantAttempts := 0
			wantError := context.Canceled
			switch phase {
			case "before request":
				cancel()
			case "abort signal":
				opts.AbortSignal = ctx
				cancel()
				ctx = t.Context()
			case "during backoff":
				wantAttempts = 1
				m.backoff = func(int) time.Duration { cancel(); return time.Hour }
			case "deadline":
				wantAttempts = 1
				wantError = context.DeadlineExceeded
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer deadlineCancel()
				m.backoff = func(int) time.Duration { return time.Hour }
			}
			_, err := m.Stream(ctx, opts)
			if !errors.Is(err, wantError) || len(model.Requests()) != wantAttempts {
				t.Fatalf("err=%v requests=%d", err, len(model.Requests()))
			}
		})
	}
}

type resetBody struct {
	io.Reader
	err error
}

func (b *resetBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, b.err
	}
	return n, err
}
func (*resetBody) Close() error { return nil }

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelCaptureResponsesHTTP2Reset(t *testing.T) {
	for _, prefix := range []string{
		"",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"unfinished\",\"name\":\"run_js\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"code\\\":\"}\n\n",
	} {
		t.Run(prefix, func(t *testing.T) {
			attempts := 0
			var bodies []string
			model := openai.NewResponsesModel("gpt-6.1-sol", openai.ResponsesConfig{
				Provider: "openai", URL: "https://provider.invalid/responses", ConfigureRequest: func(*http.Request) error { return nil },
				HTTPClient: &http.Client{Transport: responseTransport(func(r *http.Request) (*http.Response, error) {
					attempts++
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
					var response io.ReadCloser = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"))
					if attempts == 1 {
						response = &resetBody{Reader: strings.NewReader(prefix), err: http2.StreamError{StreamID: 187, Code: http2.ErrCodeInternal}}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: response}, nil
				})},
			})
			var failures []ModelStreamFailure
			m := captureForTest(model, &controller{}, &failures)
			events, err := m.Stream(t.Context(), &stream.CallOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var text string
			for event := range events {
				if e, ok := event.Data.(stream.TextDeltaEvent); ok {
					text += e.Text
				}
			}
			if text != "recovered" || attempts != 2 || bodies[0] != bodies[1] || len(failures) != 1 || !failures[0].UsageUnknown {
				t.Fatalf("text=%q attempts=%d failures=%+v", text, attempts, failures)
			}
		})
	}
}

func TestRunModelStreamRecoveryDoesNotReplayCommittedTools(t *testing.T) {
	in, model := input(batch(js("committed")), []stream.Event{
		{Type: stream.EventTextDelta, Data: stream.TextDeltaEvent{Text: "discarded"}},
		js("uncommitted"), errorEvent(transientStreamError()),
	}, batch(complete("done")))
	exec := &executor{}
	in.ExecutorFactory = func(context.Context, []capability.Definition) (jsexec.Session, error) { return exec, nil }
	result, err := Run(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply == nil || exec.calls != 1 || len(model.Requests()) != 3 {
		t.Fatalf("result=%+v tools=%d requests=%d", result, exec.calls, len(model.Requests()))
	}
	s := in.Sink.(*sink)
	if len(s.texts) != 0 || len(s.calls) != 2 || s.calls[0].ToolCallID != "committed" || s.calls[1].ToolCallID != "done" {
		t.Fatalf("sink=%+v", s)
	}
	store := in.Store.(*memoryStore)
	cp, _ := store.LoadCheckpoint(t.Context())
	if len(cp.ModelStreamFailures) != 1 || !cp.ModelStreamFailures[0].UsageUnknown {
		t.Fatalf("cp=%+v", cp)
	}
	requests := model.Requests()
	if !reflect.DeepEqual(requests[1].Messages, requests[2].Messages) {
		t.Fatal("retry changed committed transcript")
	}
	for _, msg := range store.messages {
		if strings.Contains(msg.Content, "discarded") || strings.Contains(msg.Content, "uncommitted") {
			t.Fatalf("partial response committed: %+v", msg)
		}
	}
}

func TestModelStreamBackoffBounds(t *testing.T) {
	for attempt := range modelStreamRetries {
		base := time.Second << attempt
		for range 100 {
			if delay := modelStreamBackoff(attempt); delay < base || delay >= 2*base {
				t.Fatal(delay)
			}
		}
	}
}

func TestModelCaptureRequestFailureAndActiveCancellation(t *testing.T) {
	for _, phase := range []string{"request failure", "active cancellation", "checkpoint failure"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			model, err := testutil.NewMockModel(testutil.MockConfig{ID: "request", Stream: func(ctx context.Context, _ *stream.CallOptions) (<-chan stream.Event, error) {
				if phase == "active cancellation" {
					ch := make(chan stream.Event)
					go func() { defer close(ch); cancel(); <-ctx.Done() }()
					return ch, nil
				}
				return nil, transientStreamError()
			}})
			if err != nil {
				t.Fatal(err)
			}
			var failures []ModelStreamFailure
			m := captureForTest(model, &controller{}, &failures)
			if phase == "checkpoint failure" {
				m.failed = func(context.Context, ModelStreamFailure) error { return errCrash }
			}
			_, err = m.Stream(ctx, &stream.CallOptions{})
			wantAttempts := 3
			if phase != "request failure" {
				wantAttempts = 1
			}
			if err == nil || len(model.Requests()) != wantAttempts {
				t.Fatalf("err=%v requests=%d", err, len(model.Requests()))
			}
			if phase == "active cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if phase == "checkpoint failure" && !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
		})
	}
}

// This adapter follows the host accounting contract: charge each provider finish
// before forwarding it, independently of whether response capture will commit.
type chargedModel struct {
	stream.Model
	tokens int
}

func (m *chargedModel) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	events, err := m.Model.Stream(ctx, opts)
	if err != nil {
		return nil, err
	}
	var buffered []stream.Event
	for event := range events {
		if finish, ok := event.Data.(stream.FinishEvent); ok {
			m.tokens += finish.Usage.GrandTotal()
		}
		buffered = append(buffered, event)
	}
	out := make(chan stream.Event, len(buffered))
	for _, event := range buffered {
		out <- event
	}
	close(out)
	return out, nil
}

func TestModelCaptureReportedUsageAndTokenBudget(t *testing.T) {
	for _, limit := range []int{20, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			failed := append(batch(js("discarded")), errorEvent(transientStreamError()))
			_, provider := input(failed, batch(complete("done")))
			model := &chargedModel{Model: provider}
			var failures []ModelStreamFailure
			c := &controller{}
			m := captureForTest(model, c, &failures)
			m.before = func(ctx context.Context) error {
				if model.tokens >= limit {
					return errors.New("token budget exceeded")
				}
				return c.BeforeModel(ctx)
			}
			events, err := m.Stream(t.Context(), &stream.CallOptions{})
			if limit == 20 {
				if err == nil || events != nil || model.tokens != 20 || c.steps != 1 {
					t.Fatalf("err=%v tokens=%d steps=%d", err, model.tokens, c.steps)
				}
			} else {
				if err != nil || model.tokens != 40 || c.steps != 2 {
					t.Fatalf("err=%v tokens=%d steps=%d", err, model.tokens, c.steps)
				}
			}
			if len(failures) != 1 || failures[0].UsageUnknown || failures[0].Usage.GrandTotal() != 20 {
				t.Fatal(failures)
			}
		})
	}
}

func TestRunDefinitiveFinishDoesNotRetry(t *testing.T) {
	for _, reason := range []stream.FinishReason{stream.FinishReasonContentFilter, stream.FinishReasonLength, stream.FinishReasonError, stream.FinishReasonOther, ""} {
		t.Run(string(reason), func(t *testing.T) {
			in, model := input([]stream.Event{{Type: stream.EventFinish, Data: stream.FinishEvent{FinishReason: reason, Usage: testutil.MockUsage(10, 10)}}}, batch(complete("never")))
			_, err := Run(t.Context(), in)
			if err == nil || len(model.Requests()) != 1 || len(in.Sink.(*sink).calls) != 0 {
				t.Fatalf("err=%v requests=%d", err, len(model.Requests()))
			}
		})
	}
}
