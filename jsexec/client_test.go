package jsexec

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRejectsHostileFrames(t *testing.T) {
	for _, name := range []string{"unknown_capability", "stale_id", "duplicate_call", "concurrency", "early_done", "invalid_output", "invalid_log", "oversized_exception"} {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			opts := Options{Limits: DefaultLimits(), Bindings: []Binding{{Name: "echo", Path: []string{"echo"}}}}
			opts.Limits.ConcurrentCalls = 1
			client, err := NewClient(a, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var calls atomic.Int32
			started := make(chan struct{}, 1)
			invoker := InvokerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
				calls.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				f := framed{rw: b, max: maxFrameBytes}
				m, e := f.read()
				if e != nil {
					return
				}
				call := frame{Type: "call", ID: m.ID, Call: 1, Name: "echo", Args: json.RawMessage(`[]`)}
				switch name {
				case "unknown_capability":
					call.Name = "forbidden"
					_ = f.write(call)
				case "stale_id":
					call.ID = "wrong"
					_ = f.write(call)
				case "duplicate_call", "concurrency", "early_done":
					_ = f.write(call)
					select {
					case <-started:
					case <-ctx.Done():
						return
					}
					if name == "concurrency" {
						call.Call = 2
					}
					if name == "early_done" {
						call = frame{Type: "done", ID: m.ID, Result: &Result{Undefined: true}}
					}
					_ = f.write(call)
				case "invalid_output":
					_ = f.write(frame{Type: "done", ID: m.ID, Result: &Result{}})
				case "invalid_log":
					_ = f.write(frame{Type: "done", ID: m.ID, Result: &Result{Undefined: true, Logs: []Log{{Level: "unknown"}}}})
				case "oversized_exception":
					_ = f.write(frame{Type: "done", ID: m.ID, Result: &Result{Undefined: true}, Exception: &Exception{Name: strings.Repeat("x", opts.Limits.OutputBytes+1)}})
				}
			}()
			_, err = client.Execute(ctx, `return 1`, invoker)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("hostile frame not rejected promptly: %v", err)
			}
			<-serverDone
			want := int32(0)
			if name == "duplicate_call" || name == "concurrency" || name == "early_done" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want %d", calls.Load(), want)
			}
		})
	}
}

func TestClientCancellationWaitsForCallbacks(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	options := Options{Limits: DefaultLimits(), Bindings: []Binding{{Name: "echo", Path: []string{"echo"}}}}
	client, err := NewClient(a, options)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	settled := make(chan struct{})
	go func() {
		f := framed{rw: b, max: maxFrameBytes}
		m, e := f.read()
		if e == nil {
			_ = f.write(frame{Type: "call", ID: m.ID, Call: 1, Name: "echo", Args: json.RawMessage(`[]`)})
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(ctx, `return echo()`, InvokerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(settled)
			return nil, ctx.Err()
		}))
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel hung")
	}
	select {
	case <-settled:
	default:
		t.Fatal("callback outlived Execute")
	}
}
