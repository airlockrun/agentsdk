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

// Request sends an HTTP request through Airlock's credential-injecting proxy
// and returns the raw response body. Body encoding is chosen from its type:
//
//	nil            — no body
//	[]byte, string — sent as-is
//	io.Reader      — fully read, sent as-is
//	anything else  — JSON-marshalled
//
// Returns *AuthRequiredError if the connection needs authorization.
//
// The response body may be empty (e.g. HTTP 204 No Content, which some
// upstreams use for "nothing to report"). Callers passing the result
// to json.Unmarshal must guard `len(raw) > 0` first — otherwise stdlib
// returns "unexpected end of JSON input". Use the generic
// RequestJSON helper to skip that boilerplate.
func (h *ConnectionHandle) Request(ctx context.Context, method, path string, body any) ([]byte, error) {
	bodyBytes, err := encodeProxyBody(body)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: encode proxy body: %w", err)
	}

	reqBody := ProxyRequest{Method: method, Path: path, Body: string(bodyBytes)}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	resp, err := h.agent.client.do(ctx, "POST", "/api/agent/proxy/"+h.slug, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 402 {
		var ae AuthRequiredError
		if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
			return nil, fmt.Errorf("agentsdk: 402 but failed to decode: %w", err)
		}
		return nil, &ae
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("proxy %s: status %d: %s", h.slug, resp.StatusCode, string(b))
	}
	return io.ReadAll(resp.Body)
}

// RequestJSON is the typed twin of ConnectionHandle.Request. It sends
// the request, decodes the response body into T, and returns it.
// Empty body (204 No Content, zero-length 200) decodes to a zero T
// rather than the standard library's "unexpected end of JSON input"
// — that matches the behaviour of the JS-side conn_<slug>.requestJSON
// binding, where an empty upstream body surfaces as null.
//
// Auth and HTTP-error semantics are inherited from Request: returns
// *AuthRequiredError on 402 (use IsAuthRequired to test), and an
// opaque error carrying the upstream status + body on any other
// non-2xx.
//
// Methods can't have type parameters in Go, so this is a free
// function over a *ConnectionHandle rather than a method on it.
//
//	var state PlaybackState
//	state, err := agentsdk.RequestJSON[PlaybackState](ctx, conn, "GET", "/v1/me/player", nil)
func RequestJSON[T any](ctx context.Context, h *ConnectionHandle, method, path string, body any) (T, error) {
	var zero T
	raw, err := h.Request(ctx, method, path, body)
	if err != nil {
		return zero, err
	}
	if len(raw) == 0 {
		return zero, nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("agentsdk: decode %s %s: %w", method, path, err)
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
