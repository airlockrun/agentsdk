package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/airlockrun/agentsdk/wire"
)

// airlockClient is the internal HTTP client for communicating with the Airlock API.
type airlockClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newAirlockClient(baseURL, token string, httpClient *http.Client) *airlockClient {
	return &airlockClient{
		baseURL: baseURL,
		token:   token,
		http:    httpClient,
	}
}

// newRequest creates an *http.Request with auth header set. Use when you need
// to customise headers (e.g. Content-Type) before sending.
func (c *airlockClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if c == nil {
		return nil, fmt.Errorf("agentsdk: %s %s is unavailable before the agent runtime starts", method, path)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if run := runFromContext(ctx); run != nil && run.id != "" {
		req.Header.Set("X-Airlock-Run-ID", run.id)
	}
	return req, nil
}

// do sends an HTTP request to the Airlock API with auth header.
func (c *airlockClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Credit blocking backend time to the run's go-call accumulator so the
	// L3 CPU guard doesn't mistake I/O wait for a JS spin. Nesting-safe.
	if gw := goWallFrom(ctx); gw != nil {
		gw.enter()
		defer gw.exit()
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// getRange issues a GET with a byte-range header for the inclusive range
// [start, end] (HTTP Range semantics). Mirrors do's auth header and goWall
// accounting; the caller closes resp.Body.
func (c *airlockClient) getRange(ctx context.Context, path string, start, end int64) (*http.Response, error) {
	req, err := c.newRequest(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if gw := goWallFrom(ctx); gw != nil {
		gw.enter()
		defer gw.exit()
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: GET %s (range): %w", path, err)
	}
	return resp, nil
}

// doJSON sends a JSON request and decodes the JSON response.
// Returns *AuthRequiredError on 402, *JobEnqueueUnavailableError for a typed
// enqueue-unavailable 409, and a generic error on other non-2xx responses.
func (c *airlockClient) doJSON(ctx context.Context, method, path string, reqBody, result any) error {
	return c.doJSONWithHeaders(ctx, method, path, reqBody, result, nil)
}

func (c *airlockClient) doJSONWithHeaders(ctx context.Context, method, path string, reqBody, result any, headers http.Header) error {
	if c == nil {
		return fmt.Errorf("agentsdk: %s %s is unavailable before the agent runtime starts", method, path)
	}
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("agentsdk: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}

	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if gw := goWallFrom(ctx); gw != nil {
		gw.enter()
		defer gw.exit()
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentsdk: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPaymentRequired {
		var ae AuthRequiredError
		if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
			return fmt.Errorf("agentsdk: 402 response but failed to decode auth error: %w", err)
		}
		return &ae
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict && method == http.MethodPost && path == "/api/agent/jobs" {
			var wireErr wire.EnqueueJobErrorResponse
			if err := json.Unmarshal(b, &wireErr); err == nil && wireErr.Code == wire.EnqueueJobErrorCodeUnavailable {
				return &JobEnqueueUnavailableError{
					HandlerName:    wireErr.HandlerName,
					HandlerVersion: int(wireErr.HandlerVersion),
					Message:        wireErr.Error,
				}
			}
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(b))
	}

	if result != nil && resp.ContentLength != 0 {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("agentsdk: decode response: %w", err)
		}
	}
	return nil
}
