package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// ConnectionHandle is a compile-time binding to a registered connection.
// Returned by RegisterConnection, used to make proxied HTTP requests.
type ConnectionHandle struct {
	slug  string
	agent *Agent
}

// Request sends an HTTP request through Airlock's credential-injecting
// proxy and returns the raw response body. See RequestOpts for the call
// shape and field semantics.
//
// Returns *AuthRequiredError if the connection needs authorization. The
// response body is buffered into memory and capped at
// MaxBufferedResponseBytes (20 MiB); overflow returns ErrOutputTooLarge.
// For larger responses, use RequestStream and pipe straight into storage.
//
// The response body may be empty (e.g. HTTP 204 No Content, which some
// upstreams use for "nothing to report"). Callers passing the result to
// json.Unmarshal must guard `len(raw) > 0` first — otherwise stdlib
// returns "unexpected end of JSON input". Use RequestJSON to skip that
// boilerplate.
func (h *ConnectionHandle) Request(ctx context.Context, opts RequestOpts) ([]byte, error) {
	resp, err := h.RequestStream(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// LimitReader stops AT the cap; the +1 byte tells us overflow happened.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(MaxBufferedResponseBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBufferedResponseBytes {
		return nil, ErrOutputTooLarge
	}
	return data, nil
}

// RequestStream is the streaming primitive returned to Go-only callers
// that want to process or persist a response without holding the full
// body in agent RAM. Use it for downloads, large API responses, anything
// you'd otherwise pipe through io.Copy:
//
//	resp, err := h.RequestStream(ctx, agentsdk.RequestOpts{Path: "/large.json"})
//	if err != nil { return err }
//	defer resp.Body.Close()
//	info, _ := agent.WriteFile(ctx, "tmp/large.json", resp.Body, "application/json")
//
// 402 surfaces as *AuthRequiredError; any other non-2xx becomes an opaque
// error carrying the upstream status and body preview. The returned Body
// is the live HTTP response body — close it when done.
func (h *ConnectionHandle) RequestStream(ctx context.Context, opts RequestOpts) (*ConnectionResponse, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("agentsdk: RequestOpts.Path is required")
	}
	method := opts.Method
	if method == "" {
		method = "GET"
	}
	bodyBytes, err := encodeProxyBody(opts.Body)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: encode proxy body: %w", err)
	}

	reqBody := ProxyRequest{Method: method, Path: opts.Path, Body: string(bodyBytes), Headers: opts.Headers}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := h.agent.client.do(ctx, "POST", "/api/agent/proxy/"+h.slug, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 402 {
		defer resp.Body.Close()
		var ae AuthRequiredError
		if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
			return nil, fmt.Errorf("agentsdk: 402 but failed to decode: %w", err)
		}
		return nil, &ae
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		// 4 KiB preview is enough to debug; we never need the full body
		// on the error path.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("proxy %s: status %d: %s", h.slug, resp.StatusCode, string(b))
	}
	return &ConnectionResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       resp.Body,
	}, nil
}

// RequestJSON is the typed twin of ConnectionHandle.Request. It sends
// the request, decodes the response body into T, and returns it.
// Empty body (204 No Content, zero-length 200) decodes to a zero T
// rather than the standard library's "unexpected end of JSON input"
// — matches the behaviour of the JS-side conn_<slug>.requestJSON
// binding, where an empty upstream body surfaces as null.
//
// Auth and HTTP-error semantics are inherited from Request: returns
// *AuthRequiredError on 402 (use IsAuthRequired to test), and an opaque
// error carrying the upstream status + body on any other non-2xx.
//
// Methods can't have type parameters in Go, so this is a free function
// over a *ConnectionHandle rather than a method on it.
//
//	state, err := agentsdk.RequestJSON[PlaybackState](ctx, conn,
//	    agentsdk.RequestOpts{Path: "/v1/me/player"})
func RequestJSON[T any](ctx context.Context, h *ConnectionHandle, opts RequestOpts) (T, error) {
	var zero T
	raw, err := h.Request(ctx, opts)
	if err != nil {
		return zero, err
	}
	if len(raw) == 0 {
		return zero, nil
	}
	var out T
	method := opts.Method
	if method == "" {
		method = "GET"
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("agentsdk: decode %s %s: %w", method, opts.Path, err)
	}
	return out, nil
}

func encodeProxyBody(body any) ([]byte, error) {
	switch v := body.(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	case io.Reader:
		return io.ReadAll(v)
	default:
		return json.Marshal(v)
	}
}
