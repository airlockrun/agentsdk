package agentsdk

import (
	"context"

	"github.com/airlockrun/sol/websearch"
)

// SearchProxyRequest is the body for POST /api/agent/search, shared with
// airlock (which decodes it). Slug names the registered CapSearch model slot
// whose bound provider Airlock should use; empty slug resolves to the agent's
// configured search provider (then the system default), exactly like an unbound
// model slot. The websearch.Request fields are embedded (promoted in JSON) so
// sol/websearch.Request stays provider-facing — the slug lives only on this
// envelope.
type SearchProxyRequest struct {
	Slug              string `json:"slug,omitempty"`
	Capability        string `json:"capability,omitempty"`
	websearch.Request        // embedded: fields promoted to the top-level JSON object
}

// proxySearchClient implements websearch.Client by calling
// Airlock's POST /api/agent/search endpoint. No API keys
// are exposed to the agent container.
type proxySearchClient struct {
	client *airlockClient
}

func (c *proxySearchClient) Search(ctx context.Context, slug string, req websearch.Request) (*websearch.Response, error) {
	body := SearchProxyRequest{Slug: slug, Request: req}
	if slug != "" {
		body.Capability = string(CapSearch)
	}
	var resp websearch.Response
	if err := c.client.doJSON(ctx, "POST", "/api/agent/search", body, &resp); err != nil {
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
