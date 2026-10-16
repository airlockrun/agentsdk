package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
	"google.golang.org/protobuf/types/known/structpb"
)

type ToolResult = airlockv1.InvokeDeploymentToolResponse
type AgentRun = wire.AgentRunInfo
type Binding = airlockv1.TargetBindingInfo
type Bindings = airlockv1.ListTargetBindingsResponse
type BindingSelection = airlockv1.SetTargetBindingRequest

// OpenAcceptedArtifact streams an exact ordinal in the completed run's pinned manifest.
func (c *Client) OpenAcceptedArtifact(ctx context.Context, definition, id string, index int) (io.ReadCloser, error) {
	if definition == "" || id == "" || index < 0 {
		return nil, errors.New("remote: definition, run and nonnegative artifact ordinal required")
	}
	token, err := c.config.Token(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("remote: nonempty user token required")
	}
	path := c.path() + "/agents/" + url.PathEscape(definition) + "/runs/" + url.PathEscape(id) + "/artifacts/" + strconv.Itoa(index)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := c.config.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &HTTPError{StatusCode: response.StatusCode, Body: string(data)}
	}
	return response.Body, nil
}

func relativePath(path string) (*url.URL, error) {
	u, err := url.Parse(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.Contains(path, "\\") {
		return nil, errors.New("remote: request requires an origin-relative app path")
	}
	return u, nil
}

func (c *Client) requireExecutionRevision() error {
	if c.config.Revision == "" {
		return errors.New("remote: runtime execution requires an explicit source revision")
	}
	return nil
}

func (c *Client) checkExecution(ctx context.Context) error {
	if err := c.requireExecutionRevision(); err != nil {
		return err
	}
	if c.scopedPath != "" {
		return nil
	}
	_, err := c.Get(ctx)
	return err
}

// Request drives a declared route through Airlock's real HTTP admission. The
// caller owns the response body. Redirects are returned without following them.
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	if err := c.requireExecutionRevision(); err != nil {
		return nil, err
	}
	appPath, err := relativePath(path)
	if err != nil {
		return nil, err
	}
	if err := c.checkExecution(ctx); err != nil {
		return nil, err
	}
	token, err := c.config.Token(ctx)
	if err != nil || token == "" || strings.ContainsAny(token, " \t\r\n") {
		if err == nil {
			err = errors.New("remote: nonempty user token is required")
		}
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.config.URL+c.path()+"/http"+appPath.EscapedPath(), body)
	if err != nil {
		return nil, err
	}
	request.URL.RawQuery = appPath.RawQuery
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = http.Header{}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(wire.ExpectedSourceRevisionHeader, c.config.Revision)
	client := *c.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(request)
}

func (c *Client) InvokeTool(ctx context.Context, name string, input *structpb.Value) (*ToolResult, error) {
	if err := c.requireExecutionRevision(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("remote: tool name is required")
	}
	if err := c.checkExecution(ctx); err != nil {
		return nil, err
	}
	var result airlockv1.InvokeDeploymentToolResponse
	err := c.call(ctx, http.MethodPost, c.path()+"/tools/"+url.PathEscape(name)+"/invoke", &airlockv1.InvokeDeploymentToolRequest{Input: input}, &result)
	return &result, err
}

func (c *Client) StartAgent(ctx context.Context, definition, requestID, contractHash string, input *structpb.Value) (*AgentRun, error) {
	if err := c.requireExecutionRevision(); err != nil {
		return nil, err
	}
	if definition == "" || requestID == "" {
		return nil, errors.New("remote: definition and persisted request ID are required")
	}
	if err := c.checkExecution(ctx); err != nil {
		return nil, err
	}
	return c.agent(ctx, http.MethodPost, definition, "", &airlockv1.StartDeploymentAgentRequest{RequestId: requestID, ContractHash: contractHash, Input: input})
}

func (c *Client) agent(ctx context.Context, method, definition, id string, input *airlockv1.StartDeploymentAgentRequest) (*AgentRun, error) {
	if definition == "" {
		return nil, errors.New("remote: agent definition is required")
	}
	path := c.path() + "/agents/" + url.PathEscape(definition) + "/runs"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	var result airlockv1.DeploymentAgentRunResponse
	// Avoid passing a typed nil protobuf message as a non-nil interface.
	var err error
	if input == nil {
		err = c.call(ctx, method, path, nil, &result)
	} else {
		err = c.call(ctx, method, path, input, &result)
	}
	if err != nil {
		return nil, err
	}
	if result.Run == nil {
		return nil, errors.New("remote: agent response omitted run")
	}
	raw, err := json.Marshal(result.Run.AsMap())
	if err != nil {
		return nil, err
	}
	var run wire.AgentRunInfo
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, err
	}
	if run.ID == "" || run.Definition != definition || (id != "" && run.ID != id) {
		return nil, errors.New("remote: agent response has inconsistent identity")
	}
	return &run, nil
}

func (c *Client) GetAgent(ctx context.Context, definition, id string) (*AgentRun, error) {
	if id == "" {
		return nil, errors.New("remote: run ID is required")
	}
	return c.agent(ctx, http.MethodGet, definition, id, nil)
}

func (c *Client) CancelAgent(ctx context.Context, definition, id string) (*AgentRun, error) {
	if id == "" {
		return nil, errors.New("remote: run ID is required")
	}
	return c.agent(ctx, http.MethodDelete, definition, id, nil)
}

// WaitAgent cancels observation when ctx ends; cancelling work is explicit.
func (c *Client) WaitAgent(ctx context.Context, definition, id string, poll time.Duration) (*AgentRun, error) {
	if poll <= 0 {
		return nil, errors.New("remote: positive polling interval is required")
	}
	for {
		run, err := c.GetAgent(ctx, definition, id)
		if err != nil {
			return nil, err
		}
		switch run.Status {
		case "completed", "failed", "cancelled", "budget_exceeded":
			return run, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *Client) ListBindings(ctx context.Context) (*Bindings, error) {
	var result airlockv1.ListTargetBindingsResponse
	if err := c.call(ctx, http.MethodGet, c.path()+"/bindings", nil, &result); err != nil {
		return nil, err
	}
	d, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	if result.RuntimeAgentId != d.Id {
		return nil, errors.New("remote: binding response selected a different runtime")
	}
	return &result, nil
}

func (c *Client) SetBinding(ctx context.Context, typ, slot string, selection *BindingSelection) (*Bindings, error) {
	if selection == nil || typ == "" || slot == "" || (selection.ResourceId == "") == (selection.FixtureName == "") {
		return nil, errors.New("remote: binding requires type, slot and exactly one resource or fixture")
	}
	if selection.FixtureName != "" && selection.SourceRevisionId == "" {
		return nil, errors.New("remote: fixture selection requires immutable candidate revision")
	}
	var result airlockv1.ListTargetBindingsResponse
	deployment, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.call(ctx, http.MethodPut, c.path()+"/bindings/"+url.PathEscape(typ)+"/"+url.PathEscape(slot), selection, &result); err != nil {
		return nil, err
	}
	if result.RuntimeAgentId != deployment.Id {
		return nil, errors.New("remote: binding response selected a different runtime")
	}
	return &result, nil
}
