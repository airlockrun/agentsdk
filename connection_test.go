package agentsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

// 204 No Content (or any zero-length 2xx body) from upstream is the
// failure mode that surfaced as "unexpected end of JSON input" in
// caller code that does its own json.Unmarshal on Request's bytes.
// RequestJSON skips the unmarshal on empty body and returns the
// zero T — matching what conn_<slug>.requestJSON does on the JS side.
func TestRequestJSON_EmptyBodyReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// no body
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}, phase: agentRunning}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ConnectionHandle{slug: "test", agent: a}

	type Playback struct {
		Track string `json:"track"`
	}
	got, err := RequestJSON[Playback](context.Background(), h, RequestOpts{Path: "/whatever"})
	if err != nil {
		t.Fatalf("RequestJSON returned %v, want nil on empty body", err)
	}
	if got.Track != "" {
		t.Errorf("got %+v, want zero Playback{}", got)
	}
}

// Non-empty body decodes into T as expected.
func TestRequestJSON_DecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"track":"hello"}`))
	}))
	defer srv.Close()

	a := &Agent{httpClient: &http.Client{}, phase: agentRunning}
	a.client = newAirlockClient(srv.URL, "tok", a.httpClient)
	h := &ConnectionHandle{slug: "test", agent: a}

	type Playback struct {
		Track string `json:"track"`
	}
	got, err := RequestJSON[Playback](context.Background(), h, RequestOpts{Path: "/whatever"})
	if err != nil {
		t.Fatalf("RequestJSON returned %v", err)
	}
	if got.Track != "hello" {
		t.Errorf("got %+v, want Playback{Track:\"hello\"}", got)
	}
}

func TestConnectionHTTPErrorSourceAndAuth(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		source     string
		body       string
		wantSource ConnectionErrorSource
		wantCode   string
		wantAuth   bool
	}{
		{name: "upstream 402", status: http.StatusPaymentRequired, source: "upstream", body: `{"error":"payment required"}`, wantSource: ConnectionErrorSourceUpstream},
		{name: "older host 402", status: http.StatusPaymentRequired, body: `{"code":"authorization_required","slug":"test"}`, wantSource: ConnectionErrorSourceUnknown},
		{name: "host authorization required", status: http.StatusPaymentRequired, source: "host", body: `{"error":"authorize","code":"authorization_required","slug":"test","connName":"Test API","authUrl":"https://airlock.test/auth"}`, wantAuth: true},
		{name: "host not bound", status: http.StatusNotFound, source: "host", body: `{"error":"connection not bound","code":"not_bound","slug":"test"}`, wantSource: ConnectionErrorSourceHost, wantCode: "not_bound"},
		{name: "host validation", status: http.StatusBadRequest, source: "host", body: `{"error":"invalid path","code":"invalid_request","slug":"test"}`, wantSource: ConnectionErrorSourceHost, wantCode: "invalid_request"},
		{name: "host forbidden", status: http.StatusForbidden, source: "host", body: `{"error":"forbidden","code":"forbidden","slug":"test"}`, wantSource: ConnectionErrorSourceHost, wantCode: "forbidden"},
		{name: "host gateway", status: http.StatusBadGateway, source: "host", body: `{"error":"gateway failed","code":"gateway","slug":"test"}`, wantSource: ConnectionErrorSourceHost, wantCode: "gateway"},
		{name: "upstream 401", status: http.StatusUnauthorized, source: "upstream", body: `{"error":"upstream auth"}`, wantSource: ConnectionErrorSourceUpstream},
		{name: "upstream 403", status: http.StatusForbidden, source: "upstream", body: `denied`, wantSource: ConnectionErrorSourceUpstream},
		{name: "upstream 422", status: http.StatusUnprocessableEntity, source: "upstream", body: `domain policy`, wantSource: ConnectionErrorSourceUpstream},
		{name: "upstream 500", status: http.StatusInternalServerError, source: "upstream", body: `failed`, wantSource: ConnectionErrorSourceUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.source != "" {
					w.Header().Set(wire.ConnectionResponseSourceHeader, tt.source)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			h := testConnectionHandle(srv.Client(), srv.URL)

			_, err := h.RequestStream(context.Background(), RequestOpts{Path: "/resource"})
			if tt.wantAuth {
				var authErr *AuthRequiredError
				if !errors.As(err, &authErr) || authErr.Slug != "test" {
					t.Fatalf("error = %#v, want AuthRequiredError", err)
				}
				return
			}
			var httpErr *ConnectionHTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("error = %#v, want ConnectionHTTPError", err)
			}
			if httpErr.StatusCode != tt.status || httpErr.Source != tt.wantSource || httpErr.Code != tt.wantCode || string(httpErr.Body) != tt.body {
				t.Errorf("error = %#v", httpErr)
			}
			if strings.Contains(httpErr.Error(), tt.body) {
				t.Errorf("Error() exposed response body %q", tt.body)
			}
		})
	}
}

func TestConnectionHTTPErrorPreservedByBufferedHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(wire.ConnectionResponseSourceHeader, string(wire.ConnectionResponseSourceUpstream))
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"field":"invalid"}`)
	}))
	defer srv.Close()
	h := testConnectionHandle(srv.Client(), srv.URL)

	tests := []struct {
		name string
		call func() error
	}{
		{name: "Request", call: func() error { _, err := h.Request(context.Background(), RequestOpts{Path: "/resource"}); return err }},
		{name: "RequestJSON", call: func() error {
			_, err := RequestJSON[map[string]any](context.Background(), h, RequestOpts{Path: "/resource"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var httpErr *ConnectionHTTPError
			if err := tt.call(); !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("error = %#v, want 422 ConnectionHTTPError", err)
			}
		})
	}
}

func TestConnectionHTTPErrorBodyBoundAndClosed(t *testing.T) {
	body := &trackingReadCloser{Reader: bytes.NewReader(bytes.Repeat([]byte("s"), MaxConnectionErrorBodyBytes+1))}
	h := testConnectionHandle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{wire.ConnectionResponseSourceHeader: []string{string(wire.ConnectionResponseSourceUpstream)}},
			Body:       body,
		}, nil
	})}, "http://airlock.test")

	_, err := h.RequestStream(context.Background(), RequestOpts{Path: "/resource"})
	var httpErr *ConnectionHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %#v, want ConnectionHTTPError", err)
	}
	if len(httpErr.Body) != MaxConnectionErrorBodyBytes || !httpErr.BodyTruncated {
		t.Errorf("body length = %d, truncated = %v", len(httpErr.Body), httpErr.BodyTruncated)
	}
	if !body.closed {
		t.Error("error response body was not closed")
	}
}

func TestConnectionGatewayTimeoutSupportsErrorsIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(wire.ConnectionResponseSourceHeader, string(wire.ConnectionResponseSourceHost))
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = io.WriteString(w, `{"error":"timed out","code":"gateway_timeout"}`)
	}))
	defer srv.Close()

	_, err := testConnectionHandle(srv.Client(), srv.URL).RequestStream(context.Background(), RequestOpts{Path: "/resource"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestConnectionLocalDeadlineSupportsErrorsIs(t *testing.T) {
	h := testConnectionHandle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}, "http://airlock.test")

	_, err := h.RequestStream(context.Background(), RequestOpts{Path: "/resource"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestConnectionSuccessHidesProtocolHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(wire.ConnectionResponseSourceHeader, string(wire.ConnectionResponseSourceUpstream))
		w.Header().Set("X-Upstream", "kept")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	resp, err := testConnectionHandle(srv.Client(), srv.URL).RequestStream(context.Background(), RequestOpts{Path: "/resource"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Headers.Get("X-Upstream") != "kept" {
		t.Errorf("response = %#v", resp)
	}
	if got := resp.Headers.Get(wire.ConnectionResponseSourceHeader); got != "" {
		t.Errorf("protocol header exposed as %q", got)
	}
}

func TestConnectionRequestStreamReadsLargeResponse(t *testing.T) {
	const responseSize = MaxBufferedResponseBytes + 4<<20
	wantHash := patternHash(t, responseSize)
	tests := []struct {
		name          string
		contentLength bool
	}{
		{name: "known length", contentLength: true},
		{name: "chunked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Disposition", `attachment; filename="export.bin"`)
				if tt.contentLength {
					w.Header().Set("Content-Length", strconv.Itoa(responseSize))
				}
				if _, err := io.CopyN(w, newPatternReader(), responseSize); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer srv.Close()

			resp, err := testConnectionHandle(srv.Client(), srv.URL).RequestStream(t.Context(), RequestOpts{Path: "/export"})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if got := resp.Headers.Get("Content-Disposition"); got != `attachment; filename="export.bin"` {
				t.Errorf("Content-Disposition = %q", got)
			}
			hash := sha256.New()
			written, err := io.Copy(hash, resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if written != responseSize || !bytes.Equal(hash.Sum(nil), wantHash) {
				t.Fatalf("response = %d bytes, hash %x; want %d bytes, hash %x", written, hash.Sum(nil), responseSize, wantHash)
			}
		})
	}
}

func TestConnectionBufferedMethodsRejectLargeJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":"`)
		_, _ = io.CopyN(w, repeatedByteReader('a'), MaxBufferedResponseBytes)
		_, _ = io.WriteString(w, `"}`)
	}))
	defer srv.Close()
	h := testConnectionHandle(srv.Client(), srv.URL)

	tests := []struct {
		name string
		call func() error
	}{
		{name: "Request", call: func() error {
			_, err := h.Request(t.Context(), RequestOpts{Path: "/large.json"})
			return err
		}},
		{name: "RequestJSON", call: func() error {
			_, err := RequestJSON[map[string]any](t.Context(), h, RequestOpts{Path: "/large.json"})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrOutputTooLarge) {
				t.Fatalf("error = %v, want ErrOutputTooLarge", err)
			}
		})
	}
}

func TestConnectionRequestStreamReportsPrematureResponseClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		_, _ = io.WriteString(w, "partial")
	}))
	defer srv.Close()

	resp, err := testConnectionHandle(srv.Client(), srv.URL).RequestStream(t.Context(), RequestOpts{Path: "/export"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v, want io.ErrUnexpectedEOF", err)
	}
	if string(body) != "partial" {
		t.Fatalf("body = %q, want partial", body)
	}
}

func testConnectionHandle(client *http.Client, baseURL string) *ConnectionHandle {
	a := &Agent{httpClient: client, phase: agentRunning}
	a.client = newAirlockClient(baseURL, "tok", client)
	return &ConnectionHandle{slug: "test", agent: a}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackingReadCloser struct {
	io.Reader
	closed bool
}

type patternReader struct {
	pattern [sha256.Size]byte
	offset  int
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func newPatternReader() *patternReader {
	return &patternReader{pattern: sha256.Sum256([]byte("airlock connection streaming test pattern"))}
}

func (r *patternReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.pattern[r.offset]
		r.offset = (r.offset + 1) % len(r.pattern)
	}
	return len(p), nil
}

func patternHash(t *testing.T, size int64) []byte {
	t.Helper()
	hash := sha256.New()
	if _, err := io.CopyN(hash, newPatternReader(), size); err != nil {
		t.Fatal(err)
	}
	return hash.Sum(nil)
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}
