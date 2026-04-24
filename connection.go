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

// Slug returns the connection slug.
func (h *ConnectionHandle) Slug() string { return h.slug }

// Request sends an HTTP request through Airlock's credential-injecting proxy
// and returns the raw response body. Body encoding is chosen from its type:
//
//	nil            — no body
//	[]byte, string — sent as-is
//	io.Reader      — fully read, sent as-is
//	anything else  — JSON-marshalled
//
// Returns *AuthRequiredError if the connection needs authorization.
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
		return nil, fmt.Errorf("agentsdk: proxy %s: status %d: %s", h.slug, resp.StatusCode, string(b))
	}
	return io.ReadAll(resp.Body)
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
