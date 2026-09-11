package jsexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// SupervisorOptions specifies installed, trusted runtime assets. Serve must run
// inside a network-none, finite-memory/CPU/PID container, never on an agent host.
type SupervisorOptions struct{ DenoPath, RuntimePath string }

// Serve owns one Deno subprocess and one attach stream. All subprocess stdio is
// /dev/null. Only the trusted controller isolate can connect to the local IPC
// socket; snippet workers receive an explicit permission set of "none".
func Serve(ctx context.Context, transport io.ReadWriteCloser, config SupervisorOptions) error {
	if transport == nil || config.DenoPath == "" || config.RuntimePath == "" {
		return errors.New("jsexec: supervisor dependencies required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	defer transport.Close()
	stop := context.AfterFunc(ctx, func() { _ = transport.Close() })
	defer stop()
	outer := &framed{rw: transport, max: maxFrameBytes}
	input := receive(ctx, outer)
	var first frame
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(60 * time.Second):
		return errors.New("jsexec: initialization timeout")
	case r := <-input:
		if r.err != nil {
			return r.err
		}
		first = r.frame
	}
	if first.Type != "execute" || first.Options == nil {
		return errors.New("jsexec: execute with options required")
	}
	options := *first.Options
	if err := options.validate(); err != nil {
		return err
	}
	optionsJSON, _ := json.Marshal(options)
	dir, err := os.MkdirTemp("", "jsexec-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ipc.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(10 * time.Second))
	cmd := exec.CommandContext(ctx, config.DenoPath, "run", "--no-config", "--no-lock", "--no-prompt", "--no-remote", "--no-npm", "--no-code-cache", "--allow-read="+filepath.Join(filepath.Dir(config.RuntimePath), "worker.mjs")+","+path, "--allow-write="+path, "--deny-env", "--deny-sys", "--deny-run", "--deny-ffi", "--deny-import", "--allow-net=unix:"+path, "--unstable-worker-options", config.RuntimePath, path)
	cmd.Env = []string{"DENO_DIR=" + filepath.Join(dir, "cache"), "DENO_NO_UPDATE_CHECK=1"}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited); _ = listener.Close() }()
	defer func() { _ = cmd.Process.Kill(); <-exited }()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return fmt.Errorf("jsexec: controller IPC: %w", err)
	}
	_ = listener.Close()
	defer conn.Close()
	child := &framed{rw: conn, max: options.Limits.FrameBytes}
	childStop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer childStop()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	ready, err := child.read()
	if err != nil || ready.Type != "ready" {
		return fmt.Errorf("jsexec: controller startup failed: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	baseline, err := threadCount(cmd.Process.Pid)
	if err != nil {
		return err
	}
	childInput := receive(ctx, child)
	names := map[string]bool{}
	for _, b := range options.Bindings {
		names[b.Name] = true
	}
	seenInvocations := map[string]bool{}
	next := first
	for {
		if len(seenInvocations) >= 4096 {
			return errors.New("jsexec: session invocation limit")
		}
		if next.Type != "execute" || len(next.ID) != 32 || seenInvocations[next.ID] || len(next.Code) > options.Limits.CodeBytes || next.Options == nil {
			return errors.New("jsexec: invalid invocation")
		}
		b, _ := json.Marshal(next.Options)
		if string(b) != string(optionsJSON) {
			return errors.New("jsexec: session options changed")
		}
		seenInvocations[next.ID] = true
		id := next.ID
		deadline := time.NewTimer(options.Limits.executionTimeout())
		watchdog := time.AfterFunc(options.Limits.executionTimeout(), func() { _ = cmd.Process.Kill(); _ = conn.Close(); _ = transport.Close() })
		defer watchdog.Stop()
		if err := child.write(next); err != nil {
			deadline.Stop()
			return err
		}
		pending := map[uint64]bool{}
		seen := map[uint64]bool{}
		var completion frame
		for completion.Type == "" {
			select {
			case <-ctx.Done():
				deadline.Stop()
				return ctx.Err()
			case <-deadline.C:
				_ = cmd.Process.Kill()
				_ = outer.write(frame{Type: "fatal", ID: id, Error: "execution deadline exceeded"})
				return errors.New("jsexec: execution deadline exceeded")
			case r := <-input:
				if r.err != nil {
					deadline.Stop()
					return r.err
				}
				m := r.frame
				if m.Type != "reply" || m.ID != id || !pending[m.Call] || len(m.Value) > options.Limits.OutputBytes || len(m.Error) > options.Limits.OutputBytes || m.Error == "" && !json.Valid(m.Value) {
					deadline.Stop()
					return errors.New("jsexec: invalid broker reply")
				}
				delete(pending, m.Call)
				if err := child.write(m); err != nil {
					deadline.Stop()
					return err
				}
			case r := <-childInput:
				if r.err != nil {
					deadline.Stop()
					return r.err
				}
				m := r.frame
				if m.ID != id {
					deadline.Stop()
					return errors.New("jsexec: stale child invocation")
				}
				switch m.Type {
				case "call":
					if !allowedCall(m, names) || len(m.Args) > options.Limits.OutputBytes || seen[m.Call] || len(seen) >= options.Limits.Calls || len(pending) >= options.Limits.ConcurrentCalls {
						deadline.Stop()
						return errors.New("jsexec: invalid child broker request")
					}
					seen[m.Call] = true
					pending[m.Call] = true
					if err := outer.write(m); err != nil {
						deadline.Stop()
						return err
					}
				case "done":
					if len(pending) != 0 || !validResult(m.Result, options.Limits) || !validException(m.Exception, options.Limits) {
						deadline.Stop()
						return errors.New("jsexec: invalid child completion")
					}
					completion = m
				default:
					deadline.Stop()
					return errors.New("jsexec: invalid child message")
				}
			}
		}
		deadline.Stop()
		// Additional native worker threads make reuse conservative: no background
		// worker may survive into the next invocation, including Node workers.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		threads, err := threadCount(cmd.Process.Pid)
		if err != nil || threads > baseline {
			completion.Terminal = true
			if completion.Exception == nil {
				completion.Exception = &Exception{Name: "ResourceError", Message: "worker resources prevent session reuse"}
			}
		}
		if completion.Terminal {
			_ = cmd.Process.Kill()
		}
		if err := outer.write(completion); err != nil {
			return err
		}
		watchdog.Stop()
		if completion.Terminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(options.Limits.IdleMS) * time.Millisecond):
			return errors.New("jsexec: idle deadline exceeded")
		case r := <-childInput:
			if r.err != nil {
				return r.err
			}
			return errors.New("jsexec: child message outside invocation")
		case r := <-input:
			if r.err != nil {
				return r.err
			}
			next = r.frame
		}
	}
}

func threadCount(pid int) (int, error) {
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	return len(entries), err
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return errors.Join(os.Stdin.Close(), os.Stdout.Close()) }

// Main is the cmd/jsexecutor entrypoint. It accepts no credentials or runtime
// permission switches. Container policy is the launcher's responsibility.
func Main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "jsexecutor accepts no arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := Serve(ctx, stdio{}, SupervisorOptions{DenoPath: "/usr/bin/deno", RuntimePath: "/opt/jsexec/controller.mjs"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
