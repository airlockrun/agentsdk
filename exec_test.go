package agentsdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AccessPublic on a registered exec endpoint is silently demoted to
// AccessUser. Exec is sensitive — unauthenticated callers must never
// reach it, but copy-pasting from RegisterRoute (where Public is
// meaningful) is a believable mistake worth recovering from quietly.
func TestRegisterExecEndpoint_DemotesPublicToUser(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterExecEndpoint(&ExecEndpoint{
		Slug:        "ci",
		Description: "Self-hosted CI runner",
		Access:      AccessPublic,
	})
	if got := a.execEndpoints["ci"].Access; got != AccessUser {
		t.Fatalf("expected demotion to AccessUser, got %q", got)
	}
}

// Missing Description is a programmer error — we panic loud at startup
// rather than ship an endpoint the operator UI shows blank.
func TestRegisterExecEndpoint_PanicsOnMissingDescription(t *testing.T) {
	a, _ := testAgent(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on missing Description")
		}
	}()
	a.RegisterExecEndpoint(&ExecEndpoint{Slug: "ci"})
}

// Run consumes an NDJSON stream of stdout/stderr/exit envelopes and
// reassembles them into an ExecResult. The fake airlock writes the
// envelopes in interleaved order to exercise the demuxer.
func TestExecHandle_Run_DemuxesNDJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("response writer does not support Flush")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		writeEnvelope(w, execEnvelope{Type: "stdout", Data: b64("hello ")})
		flusher.Flush()
		writeEnvelope(w, execEnvelope{Type: "stderr", Data: b64("warn\n")})
		flusher.Flush()
		writeEnvelope(w, execEnvelope{Type: "stdout", Data: b64("world")})
		flusher.Flush()
		writeEnvelope(w, execEnvelope{Type: "exit", Code: 0, DurationMs: 42})
		flusher.Flush()
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}, execEndpoints: map[string]*ExecEndpoint{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ExecHandle{slug: "vps", agent: a}

	res, err := h.Run(context.Background(), ExecCommand{Command: "echo hi"})
	if err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if string(res.Stdout) != "hello world" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello world")
	}
	if string(res.Stderr) != "warn\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "warn\n")
	}
	if res.ExitCode != 0 || res.DurationMs != 42 {
		t.Errorf("exit/duration = %d/%d, want 0/42", res.ExitCode, res.DurationMs)
	}
}

// A terminal "error" envelope mid-stream surfaces as *ExecError.
func TestExecHandle_Run_MidStreamErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeEnvelope(w, execEnvelope{Type: "stdout", Data: b64("partial")})
		writeEnvelope(w, execEnvelope{Type: "error", Kind: "transport", Message: "ssh: connection lost"})
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ExecHandle{slug: "vps", agent: a}

	_, err := h.Run(context.Background(), ExecCommand{Command: "x"})
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExecError, got %T (%v)", err, err)
	}
	if ee.Kind != "transport" || !strings.Contains(ee.Message, "connection lost") {
		t.Errorf("got %+v, want kind=transport, message contains 'connection lost'", ee)
	}
}

// Pre-stream 404 (operator hasn't configured the endpoint) surfaces as
// ExecError{Kind:"config"} so the agent author can branch on retry.
func TestExecHandle_Run_PreStreamNotConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "endpoint not configured", http.StatusNotFound)
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ExecHandle{slug: "vps", agent: a}

	_, err := h.Run(context.Background(), ExecCommand{Command: "x"})
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExecError, got %T (%v)", err, err)
	}
	if ee.Kind != "config" {
		t.Errorf("kind = %q, want config", ee.Kind)
	}
}

// Output exceeding MaxBufferedResponseBytes returns ErrOutputTooLarge.
// Run drops the partial buffer (no half-results); the caller is expected
// to retry with RunStream.
//
// Use 128 KiB chunks so each NDJSON envelope (~175 KiB on the wire after
// base64) fits comfortably under the scanner's 256 KiB max — the scanner
// is sized for the 32 KiB raw chunks airlock actually emits in
// production. Sending 160 chunks of 128 KiB = 20 MiB; one extra trips
// the +1 sentinel.
func TestExecHandle_Run_OutputTooLarge(t *testing.T) {
	chunk := strings.Repeat("a", 128*1024) // 128 KiB
	chunkB64 := b64(chunk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 161 chunks × 128 KiB = 20.125 MiB — just over the 20 MiB cap.
		for i := 0; i < 161; i++ {
			writeEnvelope(w, execEnvelope{Type: "stdout", Data: chunkB64})
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeEnvelope(w, execEnvelope{Type: "exit", Code: 0, DurationMs: 1})
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ExecHandle{slug: "vps", agent: a}

	_, err := h.Run(context.Background(), ExecCommand{Command: "cat /dev/zero"})
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("got %v, want ErrOutputTooLarge", err)
	}
}

// previewStream truncates at the byte cap and stamps a marker showing
// the original length so an operator scanning runs.actions sees what's
// missing.
func TestPreviewStream(t *testing.T) {
	short := []byte("hello")
	prev, trunc := previewStream(short, 100)
	if trunc || prev != "hello" {
		t.Errorf("short data: got (%q, %v), want (\"hello\", false)", prev, trunc)
	}

	long := []byte(strings.Repeat("a", 10000))
	prev, trunc = previewStream(long, 8192)
	if !trunc {
		t.Errorf("long data: trunc = false, want true")
	}
	if !strings.Contains(prev, "[truncated, original 10000 bytes]") {
		t.Errorf("preview missing marker: %q", prev[:200])
	}
	if !strings.HasPrefix(prev, strings.Repeat("a", 8192)) {
		t.Errorf("preview doesn't start with first 8192 bytes")
	}
}

// RunStream lets the caller stream stdout straight into an io.Writer
// without ever buffering the full body. The agent.WriteFile / io.Copy
// pattern is the headline use case.
func TestExecHandle_RunStream_PipeToWriter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			writeEnvelope(w, execEnvelope{Type: "stdout", Data: b64(fmt.Sprintf("chunk-%d ", i))})
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeEnvelope(w, execEnvelope{Type: "exit", Code: 0, DurationMs: 10})
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ExecHandle{slug: "vps", agent: a}

	stream, err := h.RunStream(context.Background(), ExecCommand{Command: "echo"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	defer stream.Stdout.Close()
	defer stream.Stderr.Close()

	got, err := io.ReadAll(stream.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if want := "chunk-0 chunk-1 chunk-2 chunk-3 chunk-4 "; string(got) != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	exit, err := stream.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.ExitCode != 0 {
		t.Errorf("exit = %+v, want code=0", exit)
	}
}

// --- helpers ---

func writeEnvelope(w io.Writer, env execEnvelope) {
	b, _ := json.Marshal(env)
	w.Write(b)
	w.Write([]byte{'\n'})
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
