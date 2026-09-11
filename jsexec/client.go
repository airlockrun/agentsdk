package jsexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Client implements Session over an owned, full-duplex attach transport.
// Closing the transport must unblock concurrent reads and writes.
type Client struct {
	f            *framed
	options      Options
	mu           sync.Mutex
	busy, closed bool
	closeDone    chan struct{}
	closeErr     error
	stop         func() bool
}

func NewClient(transport io.ReadWriteCloser, options Options) (*Client, error) {
	if transport == nil {
		return nil, errors.New("jsexec: transport required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(options)
	var copy Options
	_ = json.Unmarshal(b, &copy)
	return &Client{f: &framed{rw: transport, max: copy.Limits.FrameBytes}, options: copy, closeDone: make(chan struct{})}, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	stop := c.stop
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	c.closeErr = c.f.rw.Close()
	close(c.closeDone)
	return c.closeErr
}

func (c *Client) Execute(ctx context.Context, code string, callback Invoker) (result Result, err error) {
	if callback == nil {
		return result, errors.New("jsexec: invoker required")
	}
	if len(code) > c.options.Limits.CodeBytes {
		return result, errors.New("jsexec: code too large")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return result, ErrClosed
	}
	if c.busy {
		c.mu.Unlock()
		return result, ErrBusy
	}
	c.busy = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.busy = false; c.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, c.options.Limits.executionTimeout())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	var calls sync.WaitGroup
	defer func() { stop(); cancel(); calls.Wait() }()
	var idBytes [16]byte
	if _, err = rand.Read(idBytes[:]); err != nil {
		return result, err
	}
	id := hex.EncodeToString(idBytes[:])
	if err = c.f.write(frame{Type: "execute", ID: id, Code: code, Options: &c.options}); err != nil {
		_ = c.Close()
		return result, err
	}
	names := map[string]bool{}
	for _, b := range c.options.Bindings {
		names[b.Name] = true
	}
	completed := make(chan frame, c.options.Limits.ConcurrentCalls)
	// A single reader per invocation; completion consumes exactly one terminal
	// frame. The supervisor emits no frames between invocations.
	responses := make(chan received)
	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()
	go func() {
		for {
			m, e := c.f.read()
			select {
			case responses <- received{m, e}:
			case <-readCtx.Done():
				return
			}
			if e != nil || m.Type == "done" || m.Type == "fatal" {
				return
			}
		}
	}()
	seen := map[uint64]bool{}
	active := 0
	defer func() {
		if err != nil {
			var js *Exception
			if !errors.As(err, &js) {
				stop()
				cancel()
				_ = c.Close()
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case response := <-completed:
			active--
			if err = c.f.write(response); err != nil {
				return result, err
			}
		case incoming := <-responses:
			if incoming.err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, fmt.Errorf("jsexec: attach read: %w", incoming.err)
			}
			m := incoming.frame
			if m.ID != id {
				return result, errors.New("jsexec: invocation mismatch")
			}
			switch m.Type {
			case "call":
				if !allowedCall(m, names) || len(m.Args) > c.options.Limits.OutputBytes || seen[m.Call] || active >= c.options.Limits.ConcurrentCalls || len(seen) >= c.options.Limits.Calls {
					return result, errors.New("jsexec: invalid broker call")
				}
				seen[m.Call] = true
				active++
				calls.Add(1)
				go func() {
					defer calls.Done()
					reply := frame{Type: "reply", ID: id, Call: m.Call}
					value, e := callback.Invoke(ctx, m.Name, m.Args)
					if e != nil {
						reply.Error = e.Error()
						if reply.Error == "" {
							reply.Error = "callback failed (empty error message)"
						}
					} else if !json.Valid(value) || len(value) > c.options.Limits.OutputBytes {
						reply.Error = "invalid or oversized callback result"
					} else {
						reply.Value = value
					}
					if len(reply.Error) > c.options.Limits.OutputBytes {
						reply.Error = "callback error too large"
					}
					select {
					case completed <- reply:
					case <-ctx.Done():
					}
				}()
			case "done":
				if active != 0 || !validResult(m.Result, c.options.Limits) || !validException(m.Exception, c.options.Limits) {
					return result, errors.New("jsexec: invalid or premature completion")
				}
				result = *m.Result
				if m.Terminal {
					_ = c.Close()
				}
				if m.Exception != nil {
					return result, m.Exception
				}
				return result, nil
			case "fatal":
				return result, fmt.Errorf("jsexec: executor terminated: %s", m.Error)
			default:
				return result, errors.New("jsexec: unexpected attach frame")
			}
		}
	}
}
