package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/mcp"
)

// DirectResources serves only fixed no-auth declarations. Managed credentials
// and bound needs belong to the separately injected Resources adapter.
type DirectResources struct {
	manifest wire.AgentManifest
	client   *http.Client
}

func NewDirectResources(manifest wire.AgentManifest, client *http.Client) *DirectResources {
	if client == nil {
		panic("localruntime: direct resource HTTP client is required")
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DirectResources{manifest: clone(manifest), client: &copyClient}
}

func (p *DirectResources) Connection(ctx context.Context, _ wire.Caller, slug string, input wire.ProxyRequest) (*http.Response, error) {
	for _, need := range p.manifest.Connections {
		if need.Slug != slug {
			continue
		}
		if need.AuthMode != wire.ConnectionAuthNone {
			return nil, errors.New("localruntime: direct connection requires a no-auth declaration")
		}
		base, err := url.Parse(need.BaseURL)
		if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
			return nil, errors.New("localruntime: invalid declared connection URL")
		}
		path, err := url.Parse(input.Path)
		if err != nil || path.Host != "" || path.Scheme != "" || path.User != nil || strings.Contains(input.Path, "\\") || !strings.HasPrefix(input.Path, "/") {
			return nil, errors.New("localruntime: connection requires an origin-relative request path")
		}
		base.Path = strings.TrimRight(base.Path, "/") + path.Path
		base.RawPath = ""
		base.RawQuery = path.RawQuery
		base.Fragment = ""
		method := input.Method
		if method == "" {
			method = http.MethodGet
		}
		req, err := http.NewRequestWithContext(ctx, method, base.String(), strings.NewReader(input.Body))
		if err != nil {
			return nil, err
		}
		for key, value := range need.Headers {
			req.Header.Set(key, value)
		}
		for key, value := range input.Headers {
			req.Header.Set(key, value)
		}
		return p.client.Do(req)
	}
	return nil, fmt.Errorf("localruntime: connection %q is not declared", slug)
}

func (p *DirectResources) session(ctx context.Context, slug string) (*mcp.Session, error) {
	for _, need := range p.manifest.MCPServers {
		if need.Slug != slug {
			continue
		}
		if need.AuthMode != wire.MCPAuthNone {
			return nil, errors.New("localruntime: direct MCP requires a no-auth declaration")
		}
		return mcp.Connect(ctx, mcp.ServerConfig{Name: slug, ClientName: "agentsdk-local", Transport: "http", URL: need.URL, HTTPClient: p.client})
	}
	return nil, fmt.Errorf("localruntime: MCP %q is not declared", slug)
}

func (p *DirectResources) Discover(ctx context.Context, slug string) (schemas []wire.MCPToolSchema, err error) {
	session, err := p.session(ctx, slug)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	tools, err := session.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		input, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		var output json.RawMessage
		if t.OutputSchema != nil {
			output, err = json.Marshal(t.OutputSchema)
			if err != nil {
				return nil, err
			}
		}
		schemas = append(schemas, wire.MCPToolSchema{ServerSlug: slug, Name: t.Name, Description: t.Description, InputSchema: input, OutputSchema: output})
	}
	return schemas, nil
}

func (p *DirectResources) MCP(ctx context.Context, _ wire.Caller, slug string, input wire.MCPToolCallRequest) (out wire.MCPToolCallResponse, err error) {
	session, err := p.session(ctx, slug)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	result, err := session.CallTool(ctx, input.Tool, input.Arguments)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
