package agentsdk

import (
	"context"

	"github.com/airlockrun/sol/websearch"
)

// proxySearchClient implements websearch.Client by calling
// Airlock's POST /api/agent/search endpoint. No API keys
// are exposed to the agent container.
type proxySearchClient struct {
	client *airlockClient
}

func (c *proxySearchClient) Search(ctx context.Context, req websearch.Request) (*websearch.Response, error) {
	var resp websearch.Response
	if err := c.client.doJSON(ctx, "POST", "/api/agent/search", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// proxyHTTPClient proxies raw HTTP requests through Airlock.
type proxyHTTPClient struct {
	client *airlockClient
}

func (c *proxyHTTPClient) Do(ctx context.Context, req HTTPRequest) (*HTTPResponse, error) {
	var resp HTTPResponse
	if err := c.client.doJSON(ctx, "POST", "/api/agent/http", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
