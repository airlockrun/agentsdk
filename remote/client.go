// Package remote is an explicit user-authenticated client for real Airlock
// deployment targets. It does not start an application, database or local host.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Deployment is the generated operator contract, shared with the Airlock API.
type Deployment = airlockv1.DeploymentInfo
type Policy = airlockv1.SetDeploymentPolicyRequest

// Config identifies one logical application on one Airlock instance. Target is
// production by default; previews use a concrete deployment name or ID. Revision, when set,
// pins observation to that immutable source revision. Tokens are user/operator
// credentials, never SDK app credentials or caller assertions.
type Config struct {
	URL, AppID, Target, Revision string
	HTTPClient                   *http.Client
	Token                        func(context.Context) (string, error)
}

type Client struct {
	config     Config
	scopedPath string
}

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("remote: explicit HTTP(S) Airlock origin is required")
	}
	if config.Target == "" {
		config.Target = "production"
	}
	if config.AppID == "" || strings.ContainsAny(config.AppID+config.Target, "/\\?# \t\r\n") || config.Token == nil || config.HTTPClient == nil {
		return nil, errors.New("remote: app, user token provider and HTTP client are required")
	}
	if config.Target == "preview" {
		return nil, errors.New("remote: preview requires a concrete deployment name or ID")
	}
	config.URL = strings.TrimRight(config.URL, "/")
	return &Client{config: config}, nil
}

func (c *Client) path() string {
	if c.scopedPath != "" {
		return c.scopedPath
	}
	return "/api/v1/agents/" + url.PathEscape(c.config.AppID) + "/deployments/" + url.PathEscape(c.config.Target)
}

// HTTPError preserves API status without falling back to another target.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("remote: HTTP %d: %s", e.StatusCode, e.Body) }

func (c *Client) call(ctx context.Context, method, path string, input, output proto.Message) error {
	var body []byte
	var err error
	if input != nil {
		body, err = protojson.Marshal(input)
		if err != nil {
			return err
		}
	}
	token, err := c.config.Token(ctx)
	if err != nil {
		return err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("remote: nonempty user token is required")
	}
	request, err := http.NewRequestWithContext(ctx, method, c.config.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if c.config.Revision != "" {
		request.Header.Set(wire.ExpectedSourceRevisionHeader, c.config.Revision)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *c.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("remote: operator response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if output == nil {
		return nil
	}
	return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, output)
}

func (c *Client) deployment(ctx context.Context, method, path string, input proto.Message) (*Deployment, error) {
	var response airlockv1.GetDeploymentResponse
	if err := c.call(ctx, method, path, input, &response); err != nil {
		return nil, err
	}
	d := response.Deployment
	if d == nil || d.AppId != c.config.AppID || d.Id == "" || d.Id != d.RuntimeAgentId {
		return nil, errors.New("remote: deployment response has inconsistent identity")
	}
	if method == http.MethodPost && strings.HasSuffix(path, "/deployments/preview") {
		if d.Kind != "personal_preview" || d.Name == "preview" || d.Name == "production" || d.Name == "" {
			return nil, errors.New("remote: preview response is not a concrete deployment")
		}
	} else if d.Name != c.config.Target && d.Id != c.config.Target {
		return nil, errors.New("remote: deployment response selected a different target")
	}
	if c.config.Revision != "" && d.SourceRevisionId != c.config.Revision {
		return nil, errors.New("remote: deployment source revision changed")
	}
	return d, nil
}

func (c *Client) Get(ctx context.Context) (*Deployment, error) {
	if c.scopedPath != "" {
		return nil, errors.New("runner capabilities have no deployment discovery/management API")
	}
	return c.deployment(ctx, http.MethodGet, c.path(), nil)
}

// CreatePreview creates an independent creator-associated deployment. Select its
// returned concrete name or ID in a new client for subsequent operations.
func (c *Client) CreatePreview(ctx context.Context) (*Deployment, error) {
	if c.scopedPath != "" || c.config.Revision != "" {
		return nil, errors.New("remote: creation requires unpinned user authentication")
	}
	path := "/api/v1/agents/" + url.PathEscape(c.config.AppID) + "/deployments/preview"
	return c.deployment(ctx, http.MethodPost, path, &airlockv1.CreatePreviewRequest{})
}

func (c *Client) SetPolicy(ctx context.Context, policy *Policy) (*Deployment, error) {
	if policy == nil {
		return nil, errors.New("remote: explicit deployment policy is required")
	}
	return c.deployment(ctx, http.MethodPut, c.path()+"/policy", policy)
}

// Lifecycle changes only the selected deployment. Data/bindings are retained by
// the host. Refresh Get separately to inspect actual running state.
func (c *Client) Lifecycle(ctx context.Context, action string) error {
	if action != "start" && action != "stop" && action != "suspend" {
		return errors.New("remote: lifecycle action must be start, stop or suspend")
	}
	if _, err := c.Get(ctx); err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, c.path()+"/"+action, nil, nil)
}
