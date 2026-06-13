package agentsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ExecHandle is the compile-time-bound handle returned by RegisterExecEndpoint.
// Use Run for buffered convenience (capped at MaxBufferedResponseBytes,
// surfaces ErrOutputTooLarge on overflow); use RunStream when the output is
// data you want to pipe straight into storage without holding it in agent RAM.
type ExecHandle struct {
	slug  string
	agent *Agent
}

// Slug returns the endpoint's registered slug. Useful for log lines and
// error wrapping when the caller has a generic *ExecHandle.
func (h *ExecHandle) Slug() string { return h.slug }

// MaxBufferedResponseBytes is the cap Run enforces per stream. Mirrors the
// airlock-side constant of the same name. Documented here so tools that
// wrap Run can surface the limit in their own error messages without
// importing airlock packages.
const MaxBufferedResponseBytes = 20 << 20 // 20 MiB

// execRecordPreviewBytes is the per-stream slice kept in runs.actions
// JSONB for audit visibility. The caller of Run still gets the full data;
// only the recorded preview is truncated.
const execRecordPreviewBytes = 8 * 1024 // 8 KiB

// execEnvelope is one line of the NDJSON streaming response.
//
// type=stdout|stderr → Data is base64-encoded bytes
// type=exit          → Code + DurationMs populated; stream ends after this
// type=error         → Kind + Message populated; terminal failure mid-stream
type execEnvelope struct {
	Type       string `json:"type"`
	Data       string `json:"data,omitempty"`
	Code       int    `json:"code,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Message    string `json:"message,omitempty"`
}

// recordedExecRequest is what lands in runs.actions for an exec call.
// Stdin is replaced with a length-only placeholder — stdin can carry
// secrets and the audit log isn't the place to put them.
type recordedExecRequest struct {
	Slug      string   `json:"slug"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	StdinLen  int      `json:"stdinLen,omitempty"`
	TimeoutMs int64    `json:"timeoutMs,omitempty"`
}

// recordedExecResponse is the audit payload. stdout/stderr are truncated
// to execRecordPreviewBytes; the caller still has the full data via the
// returned ExecResult.
type recordedExecResponse struct {
	ExitCode        int    `json:"exitCode"`
	DurationMs      int64  `json:"durationMs"`
	StdoutPreview   string `json:"stdoutPreview,omitempty"`
	StderrPreview   string `json:"stderrPreview,omitempty"`
	StdoutBytes     int    `json:"stdoutBytes"`
	StderrBytes     int    `json:"stderrBytes"`
	StdoutTruncated bool   `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool   `json:"stderrTruncated,omitempty"`
}

// RunStream opens an exec session over the agent's airlock client and
// returns reader handles for stdout/stderr plus a Wait function that
// blocks until the remote sends its exit envelope.
//
// The caller MUST close both Stdout and Stderr even if it only consumes
// one — the background demux goroutine drives both, and a half-drained
// stream blocks the other side. The simplest idiom is:
//
//	s, err := h.RunStream(ctx, cmd)
//	if err != nil { return err }
//	defer s.Stdout.Close()
//	defer s.Stderr.Close()
func (h *ExecHandle) RunStream(ctx context.Context, cmd ExecCommand) (*ExecStream, error) {
	if cmd.Command == "" {
		return nil, &ExecError{Kind: "config", Message: "Command is required"}
	}

	reqBody := ExecRequest{
		Command:   cmd.Command,
		Args:      cmd.Args,
		TimeoutMs: cmd.Timeout.Milliseconds(),
	}
	if len(cmd.Stdin) > 0 {
		reqBody.StdinB64 = base64.StdEncoding.EncodeToString(cmd.Stdin)
	}
	wire, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: marshal exec request: %w", err)
	}

	resp, err := h.agent.client.do(ctx, "POST", "/api/agent/exec/"+h.slug, bytes.NewReader(wire))
	if err != nil {
		return nil, &ExecError{Kind: "transport", Message: err.Error()}
	}

	// Pre-stream errors come back as standard HTTP status codes (404 for
	// unconfigured slug, 501 for unsupported transport, 400 for validation,
	// 502 for transport failure before any stdout arrived). Once the
	// stream starts (chunked body), we read 200; mid-stream failures land
	// as terminal "error" envelopes inside the body.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		kind := preStreamErrorKind(resp.StatusCode)
		return nil, &ExecError{Kind: kind, Message: fmt.Sprintf("exec %s: status %d: %s", h.slug, resp.StatusCode, string(body))}
	}

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	exitCh := make(chan ExecExit, 1)
	errCh := make(chan error, 1)

	go demuxExecStream(resp, stdoutW, stderrW, exitCh, errCh)

	stream := &ExecStream{
		Stdout: stdoutR,
		Stderr: stderrR,
		Wait: func() (ExecExit, error) {
			select {
			case exit := <-exitCh:
				return exit, nil
			case err := <-errCh:
				return ExecExit{}, err
			}
		},
	}
	return stream, nil
}

// preStreamErrorKind classifies pre-stream HTTP status codes into the
// ExecError.Kind taxonomy. Mid-stream errors carry their own kind from
// the "error" envelope.
func preStreamErrorKind(status int) string {
	switch status {
	case http.StatusNotFound:
		return "config" // endpoint slug not configured by operator
	case http.StatusNotImplemented:
		return "config" // transport not supported (e.g. telnet in v1)
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusGatewayTimeout:
		return "timeout"
	default:
		return "transport"
	}
}

// demuxExecStream is the goroutine that scans NDJSON envelopes from the
// response body and routes them to the two pipe writers. Closes the
// pipes (with or without an error) on exit, signals via exitCh/errCh.
func demuxExecStream(resp *http.Response, stdoutW, stderrW *io.PipeWriter, exitCh chan<- ExecExit, errCh chan<- error) {
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	// Allow envelopes up to slightly more than one full base64-encoded chunk;
	// airlock writes 32 KiB raw → ~43 KiB base64 → ~44 KiB envelope.
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	closeErr := func(err error) {
		stdoutW.CloseWithError(err)
		stderrW.CloseWithError(err)
		errCh <- err
	}

	for scanner.Scan() {
		var env execEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			closeErr(&ExecError{Kind: "transport", Message: "malformed exec envelope: " + err.Error()})
			return
		}
		switch env.Type {
		case "stdout":
			data, err := base64.StdEncoding.DecodeString(env.Data)
			if err != nil {
				closeErr(&ExecError{Kind: "transport", Message: "stdout decode: " + err.Error()})
				return
			}
			if _, err := stdoutW.Write(data); err != nil {
				// Pipe reader closed by caller. Drain the rest of the
				// stream by reading and discarding until we see the
				// exit envelope, so the airlock-side writer doesn't
				// block forever.
				drainExecStream(scanner, stderrW, exitCh, errCh)
				return
			}
		case "stderr":
			data, err := base64.StdEncoding.DecodeString(env.Data)
			if err != nil {
				closeErr(&ExecError{Kind: "transport", Message: "stderr decode: " + err.Error()})
				return
			}
			if _, err := stderrW.Write(data); err != nil {
				drainExecStream(scanner, stdoutW, exitCh, errCh)
				return
			}
		case "exit":
			stdoutW.Close()
			stderrW.Close()
			exitCh <- ExecExit{ExitCode: env.Code, DurationMs: env.DurationMs}
			return
		case "error":
			closeErr(&ExecError{Kind: nonEmpty(env.Kind, "transport"), Message: env.Message})
			return
		default:
			// Forward-compat: ignore unknown envelope types so a future
			// airlock can extend the protocol without breaking older
			// SDK consumers.
		}
	}
	if err := scanner.Err(); err != nil {
		closeErr(&ExecError{Kind: "transport", Message: "stream read: " + err.Error()})
		return
	}
	// Stream ended without an exit envelope — treat as transport failure.
	closeErr(&ExecError{Kind: "transport", Message: "exec stream ended without exit envelope"})
}

// drainExecStream is called when one of the pipe writers fails (the
// caller closed its reader half). We still need to drain the airlock
// response so its writer goroutines can finish; we deliver the exit /
// error envelope to the channels.
func drainExecStream(scanner *bufio.Scanner, otherW *io.PipeWriter, exitCh chan<- ExecExit, errCh chan<- error) {
	otherW.Close()
	for scanner.Scan() {
		var env execEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			errCh <- &ExecError{Kind: "transport", Message: "malformed exec envelope (draining): " + err.Error()}
			return
		}
		switch env.Type {
		case "exit":
			exitCh <- ExecExit{ExitCode: env.Code, DurationMs: env.DurationMs}
			return
		case "error":
			errCh <- &ExecError{Kind: nonEmpty(env.Kind, "transport"), Message: env.Message}
			return
		}
	}
	if err := scanner.Err(); err != nil {
		errCh <- &ExecError{Kind: "transport", Message: "stream read (draining): " + err.Error()}
		return
	}
	errCh <- &ExecError{Kind: "transport", Message: "exec stream ended without exit envelope (draining)"}
}

func nonEmpty(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

// Run is the buffered convenience wrapper around RunStream. It reads
// both streams up to MaxBufferedResponseBytes each, then waits for the
// exit envelope. Overflow on either stream returns ErrOutputTooLarge
// with no partial ExecResult — use RunStream and pipe straight into
// agent.WriteFile if you expect larger output.
//
// Non-zero exit codes are NOT errors — inspect ExecResult.ExitCode and
// ExecResult.Stderr. Errors returned by Run are *ExecError for transport
// failures, ErrOutputTooLarge for buffer overflow, or context errors.
func (h *ExecHandle) Run(ctx context.Context, cmd ExecCommand) (ExecResult, error) {
	start := time.Now()
	stream, err := h.RunStream(ctx, cmd)
	if err != nil {
		h.recordAction(ctx, cmd, ExecResult{}, err, time.Since(start))
		return ExecResult{}, err
	}
	defer stream.Stdout.Close()
	defer stream.Stderr.Close()

	// Drain stdout and stderr in parallel — they share one demux
	// goroutine on the airlock side, and a sequential read would
	// deadlock the moment one stream's pipe-write blocked on a slow
	// reader of the other. On overflow we close the pipe so the demux
	// goroutine's next Write fails with io.ErrClosedPipe and falls into
	// drainExecStream, which lets Wait return cleanly.
	type readOut struct {
		data []byte
		err  error
	}
	stdoutCh := make(chan readOut, 1)
	stderrCh := make(chan readOut, 1)
	go func() {
		d, e := readWithCap(stream.Stdout, MaxBufferedResponseBytes)
		if errors.Is(e, ErrOutputTooLarge) {
			stream.Stdout.Close()
		}
		stdoutCh <- readOut{d, e}
	}()
	go func() {
		d, e := readWithCap(stream.Stderr, MaxBufferedResponseBytes)
		if errors.Is(e, ErrOutputTooLarge) {
			stream.Stderr.Close()
		}
		stderrCh <- readOut{d, e}
	}()
	out := <-stdoutCh
	errOut := <-stderrCh
	stdout, stdoutErr := out.data, out.err
	stderr, stderrErr := errOut.data, errOut.err

	exit, waitErr := stream.Wait()

	if errors.Is(stdoutErr, ErrOutputTooLarge) || errors.Is(stderrErr, ErrOutputTooLarge) {
		h.recordAction(ctx, cmd, ExecResult{}, ErrOutputTooLarge, time.Since(start))
		return ExecResult{}, ErrOutputTooLarge
	}
	if stdoutErr != nil {
		h.recordAction(ctx, cmd, ExecResult{}, stdoutErr, time.Since(start))
		return ExecResult{}, stdoutErr
	}
	if stderrErr != nil {
		h.recordAction(ctx, cmd, ExecResult{}, stderrErr, time.Since(start))
		return ExecResult{}, stderrErr
	}
	if waitErr != nil {
		h.recordAction(ctx, cmd, ExecResult{}, waitErr, time.Since(start))
		return ExecResult{}, waitErr
	}

	res := ExecResult{
		Stdout:     stdout,
		Stderr:     stderr,
		ExitCode:   exit.ExitCode,
		DurationMs: exit.DurationMs,
	}
	h.recordAction(ctx, cmd, res, nil, time.Since(start))
	return res, nil
}

// readWithCap reads up to maxBytes from r. If r has more data than that,
// returns ErrOutputTooLarge and the partial buffer is discarded.
func readWithCap(r io.Reader, maxBytes int) ([]byte, error) {
	// LimitReader stops AT maxBytes; the +1 sentinel byte tells us
	// overflow happened. A read of exactly maxBytes is fine.
	lr := io.LimitReader(r, int64(maxBytes)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, ErrOutputTooLarge
	}
	return data, nil
}

// recordAction stores an audit entry on the current run if one exists.
// Crons/webhooks may run without a *run in ctx — that's a no-op here.
func (h *ExecHandle) recordAction(ctx context.Context, cmd ExecCommand, res ExecResult, err error, duration time.Duration) {
	r := runFromContext(ctx)
	if r == nil {
		return
	}
	req := recordedExecRequest{
		Slug:      h.slug,
		Command:   cmd.Command,
		Args:      cmd.Args,
		StdinLen:  len(cmd.Stdin),
		TimeoutMs: cmd.Timeout.Milliseconds(),
	}
	stdoutPreview, stdoutTrunc := previewStream(res.Stdout, execRecordPreviewBytes)
	stderrPreview, stderrTrunc := previewStream(res.Stderr, execRecordPreviewBytes)
	resp := recordedExecResponse{
		ExitCode:        res.ExitCode,
		DurationMs:      res.DurationMs,
		StdoutPreview:   stdoutPreview,
		StderrPreview:   stderrPreview,
		StdoutBytes:     len(res.Stdout),
		StderrBytes:     len(res.Stderr),
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
	}
	r.recordAction("exec", req, resp, err, duration)
}

// previewStream returns the first cap bytes of data with a truncation
// marker if data was longer. The marker is human-readable and shows the
// original byte count so an operator scanning runs.actions can see what
// they're missing.
func previewStream(data []byte, cap int) (string, bool) {
	if len(data) <= cap {
		return string(data), false
	}
	return string(data[:cap]) + fmt.Sprintf("\n... [truncated, original %d bytes]\n", len(data)), true
}
