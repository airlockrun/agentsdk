package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	goaierrors "github.com/airlockrun/goai/errors"
	"github.com/airlockrun/goai/stream"
)

// modelCapture retries only uncommitted model responses. Native tools are
// dispatched by Run after the complete response is checkpointed, never here.
// Keeping events private also prevents duplicated text or tool-input fragments.
type modelCapture struct {
	stream.Model
	before        func(context.Context) error
	failed        func(context.Context, ModelStreamFailure) error
	backoff       func(int) time.Duration
	firstReserved bool
}

const modelStreamRetries = 2

func modelStreamBackoff(attempt int) time.Duration {
	base := time.Second << attempt
	return base + time.Duration(rand.Int64N(int64(base)))
}

func (m *modelCapture) Stream(ctx context.Context, opts *stream.CallOptions) (<-chan stream.Event, error) {
	if opts == nil {
		return nil, errors.New("agentruntime: model call options are required")
	}
	if opts.AbortSignal != nil {
		combined, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(opts.AbortSignal, cancel)
		defer stop()
		defer cancel()
		if opts.AbortSignal.Err() != nil {
			cancel()
		}
		ctx = combined
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt != 0 || !m.firstReserved {
			if err := m.before(ctx); err != nil {
				return nil, err
			}
		}
		// Cancel each provider request independently, including blocked producers
		// after an error. A fresh request uses the same committed conversation.
		requestCtx, cancel := context.WithCancel(ctx)
		copy := *opts
		copy.AbortSignal = requestCtx
		events, err := m.Model.Stream(requestCtx, &copy)
		var buffered []stream.Event
		var usage stream.Usage
		finished, externalEffect := false, false
		if err == nil {
			if events == nil {
				err = errors.New("agentruntime: model returned nil stream")
			} else {
			read:
				for {
					select {
					case <-ctx.Done():
						err = ctx.Err()
						break read
					case event, ok := <-events:
						if !ok {
							break read
						}
						buffered = append(buffered, event)
						switch e := event.Data.(type) {
						case stream.FinishEvent:
							if finished {
								err = errors.New("agentruntime: duplicate model finish")
								break read
							}
							finished, usage = true, e.Usage.Normalized()
						case stream.FinishStepEvent:
							usage = e.Usage.Normalized()
						case stream.ToolCallEvent:
							externalEffect = externalEffect || e.ProviderExecuted
						case stream.ToolInputStartEvent:
							externalEffect = externalEffect || e.ProviderExecuted
						case stream.ToolResultEvent:
							externalEffect = true
						case stream.ToolErrorEvent:
							externalEffect = true
						case stream.ErrorEvent:
							err = e.Error
							if err == nil {
								err = errors.New("agentruntime: empty model stream error")
							}
							break read
						}
					}
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil && !finished {
			err = fmt.Errorf("%w: model stream closed without finish", goaierrors.ErrInvalidResponse)
		}
		if err == nil && externalEffect {
			err = errors.New("agentruntime: provider-executed tools are not supported")
		}
		if err == nil {
			out := make(chan stream.Event, len(buffered))
			for _, event := range buffered {
				out <- event
			}
			close(out)
			return out, nil
		}
		// Usage belongs to each attempted request, including failed ones. Hosts
		// charge reported usage at their model adapter; this journal preserves
		// unknown totals instead of claiming a failed request cost zero tokens.
		if saveErr := m.failed(ctx, ModelStreamFailure{
			Usage: usage, UsageUnknown: usage.InputTokens.Total == nil || usage.OutputTokens.Total == nil,
		}); saveErr != nil {
			return nil, errors.Join(err, saveErr)
		}
		var apiErr *goaierrors.APICallError
		if attempt >= modelStreamRetries || externalEffect || ctx.Err() != nil ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			!errors.As(err, &apiErr) || !apiErr.IsRetryable ||
			(apiErr.StatusCode != 0 && apiErr.StatusCode != 408 && apiErr.StatusCode != 429 && apiErr.StatusCode < 500) {
			return nil, err
		}
		timer := time.NewTimer(m.backoff(attempt))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}
