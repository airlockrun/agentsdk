package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/airlockrun/agentsdk/capability"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
)

// localRemoteResources retains the CLI session only in the host. Airlock checks
// the live session, app admin access and exact current binding on every call.
type localRemoteResources struct {
	target          integrationTarget
	developerID     string
	token           func(context.Context) (string, error)
	localCredential bool
}

func (p *localRemoteResources) path(caller wire.Caller, suffix string) (string, error) {
	if err := caller.Validate(); err != nil {
		return "", err
	}
	if caller.Kind == "user" {
		if caller.User.ID != p.developerID {
			return "", errors.New("local developer identity does not match the authenticated resource session")
		}
		if p.localCredential {
			return "/api/local/agents/" + url.PathEscape(p.target.agentID) + "/integrations" + suffix, nil
		}
		return p.target.path(suffix), nil
	}
	if caller.Kind != "application" || caller.Initiator != nil {
		return "", errors.New("remote resources require an acting developer or application-owned shared call")
	}
	if p.localCredential {
		return "/api/local/agents/" + url.PathEscape(p.target.agentID) + "/integrations/application" + suffix, nil
	}
	return p.target.path("/application" + suffix), nil
}

type localCredentialCoordinates struct {
	UserID    string `json:"sub"`
	SessionID string `json:"sid"`
	AppID     string `json:"agent_id"`
	Profile   string `json:"token_use"`
}

func localCredentialScope(token string) (localCredentialCoordinates, error) {
	// Decoding preserves session continuity only. Airlock validates the signature,
	// token profile, target app and live authority at every resource operation.
	var scope localCredentialCoordinates
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return scope, errors.New("invalid local resource admission response")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return scope, err
	}
	if err := json.Unmarshal(raw, &scope); err != nil {
		return scope, err
	}
	if scope.UserID == "" || scope.SessionID == "" || scope.AppID == "" || scope.Profile != "local_resources" {
		return scope, errors.New("invalid local resource admission coordinates")
	}
	return scope, nil
}

func admitLocalResourceToken(ctx context.Context, target integrationTarget, developerToken string) (string, localCredentialCoordinates, error) {
	var out airlockv1.RefreshResponse
	err := doProto(ctx, target.baseURL, "POST", target.path("/local-resource-token"), developerToken, nil, &out)
	if err != nil {
		return "", localCredentialCoordinates{}, err
	}
	scope, err := localCredentialScope(out.AccessToken)
	return out.AccessToken, scope, err
}

func (p *localRemoteResources) Connection(ctx context.Context, caller wire.Caller, slug string, req wire.ProxyRequest) (*http.Response, error) {
	path, err := p.path(caller, "/connections/"+url.PathEscape(slug)+"/request")
	if err != nil {
		return nil, err
	}
	token, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	var out airlockv1.InvokeConnectionResponse
	if err := doProto(ctx, p.target.baseURL, "POST", path, token, &airlockv1.InvokeConnectionRequest{Method: req.Method, Path: req.Path, Body: []byte(req.Body), Headers: req.Headers}, &out); err != nil {
		return nil, err
	}
	headers := http.Header{}
	for _, header := range out.Headers {
		headers[header.Name] = header.Values
	}
	return &http.Response{StatusCode: int(out.StatusCode), Header: headers, Body: io.NopCloser(bytes.NewReader(out.Body)), ContentLength: int64(len(out.Body))}, nil
}

func (p *localRemoteResources) MCP(ctx context.Context, caller wire.Caller, slug string, req wire.MCPToolCallRequest) (wire.MCPToolCallResponse, error) {
	path, err := p.path(caller, "/mcp/"+url.PathEscape(slug)+"/call")
	if err != nil {
		return wire.MCPToolCallResponse{}, err
	}
	token, err := p.token(ctx)
	if err != nil {
		return wire.MCPToolCallResponse{}, err
	}
	var out airlockv1.InvokeMCPToolResponse
	if err := doProto(ctx, p.target.baseURL, "POST", path, token, &airlockv1.InvokeMCPToolRequest{Tool: req.Tool, ArgumentsJson: req.Arguments}, &out); err != nil {
		return wire.MCPToolCallResponse{}, err
	}
	result := wire.MCPToolCallResponse{IsError: out.IsError, StructuredContent: out.StructuredContentJson, Meta: out.MetaJson}
	for _, content := range out.Content {
		var value wire.MCPContent
		if err := json.Unmarshal(content.ContentJson, &value); err != nil {
			return wire.MCPToolCallResponse{}, err
		}
		result.Content = append(result.Content, value)
	}
	return result, nil
}

func (p *localRemoteResources) discovery(ctx context.Context, manifest wire.AgentManifest, user wire.CallerUser) (capability.Discovery, error) {
	out := capability.Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{}}
	for _, need := range manifest.MCPServers {
		if need.AuthMode == wire.MCPAuthNone {
			continue
		}
		caller := wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: "background"}}
		if need.BindingMode == wire.BindingPerUser {
			caller = wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}
		}
		path, err := p.path(caller, "/mcp/"+url.PathEscape(need.Slug)+"/tools")
		if err != nil {
			return out, err
		}
		token, err := p.token(ctx)
		if err != nil {
			return out, err
		}
		var schemas airlockv1.ListIntegrationMCPToolsResponse
		if err := doProto(ctx, p.target.baseURL, "GET", path, token, nil, &schemas); err != nil {
			return out, err
		}
		for _, schema := range schemas.Tools {
			out.MCPSchemas[need.Slug] = append(out.MCPSchemas[need.Slug], wire.MCPToolSchema{ServerSlug: need.Slug, Name: schema.Name, Description: schema.Description, InputSchema: schema.InputSchemaJson, OutputSchema: schema.OutputSchemaJson})
		}
	}
	return out, nil
}
