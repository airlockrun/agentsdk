package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// do sends an HTTP request to the Airlock API with auth header.
func (c *airlockClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: request %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// doJSON sends a JSON request and decodes the JSON response.
// Returns *AuthRequiredError on 402, generic error on non-2xx.
func (c *airlockClient) doJSON(ctx context.Context, method, path string, reqBody, result any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("agentsdk: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}

	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
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
		return fmt.Errorf("agentsdk: %s %s: status %d: %s", method, path, resp.StatusCode, string(b))
	}

	if result != nil && resp.ContentLength != 0 {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("agentsdk: decode response: %w", err)
		}
	}
	return nil
}
