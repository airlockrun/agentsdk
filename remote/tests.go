package remote

import (
	"context"
	"errors"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"net/http"
	"net/url"
	"time"
)

type TestRun = airlockv1.DeploymentTestRunInfo
type TestRequest = airlockv1.RunDeploymentTestsRequest

func (c *Client) testRun(ctx context.Context, method, path string, request *TestRequest) (*TestRun, error) {
	var out airlockv1.DeploymentTestRunResponse
	var err error
	if request == nil {
		err = c.call(ctx, method, path, nil, &out)
	} else {
		err = c.call(ctx, method, path, request, &out)
	}
	if err != nil {
		return nil, err
	}
	info := out.Test
	if info == nil || !canonicalID(info.Id) || info.AppId != c.config.AppID || info.SourceRevisionId != c.config.Revision || !canonicalID(info.DeploymentId) {
		return nil, errors.New("test run response has inconsistent target/revision identity")
	}
	return info, nil
}
func (c *Client) RunTests(ctx context.Context, request *TestRequest) (*TestRun, error) {
	if c.scopedPath != "" || request == nil || !canonicalID(c.config.Revision) || request.SourceRevisionId != c.config.Revision {
		return nil, errors.New("suite admission requires user-authenticated preview and exact source revision")
	}
	if request.TimeoutSeconds < 0 || request.TimeoutSeconds > 600 {
		return nil, errors.New("test timeout must be at most 600 seconds")
	}
	deployment, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	if deployment.Kind != "personal_preview" {
		return nil, errors.New("suite admission requires a selected preview deployment")
	}
	result, err := c.testRun(ctx, http.MethodPost, c.path()+"/tests", request)
	if err == nil && result.DeploymentId != deployment.Id {
		return nil, errors.New("test admission selected a different deployment")
	}
	return result, err
}
func (c *Client) GetTest(ctx context.Context, id string) (*TestRun, error) {
	if !canonicalID(id) {
		return nil, errors.New("test ID must be canonical UUID")
	}
	result, err := c.testRun(ctx, http.MethodGet, c.path()+"/tests/"+url.PathEscape(id), nil)
	if err == nil && result.Id != id {
		return nil, errors.New("test response selected a different run")
	}
	return result, err
}
func (c *Client) CancelTest(ctx context.Context, id string) (*TestRun, error) {
	if !canonicalID(id) {
		return nil, errors.New("test ID must be canonical UUID")
	}
	result, err := c.testRun(ctx, http.MethodDelete, c.path()+"/tests/"+url.PathEscape(id), nil)
	if err == nil && result.Id != id {
		return nil, errors.New("test response selected a different run")
	}
	return result, err
}
func (c *Client) WaitTest(ctx context.Context, id string, poll time.Duration) (*TestRun, error) {
	if poll <= 0 {
		return nil, errors.New("test polling interval must be positive")
	}
	var deployment string
	for {
		info, err := c.GetTest(ctx, id)
		if err != nil {
			return nil, err
		}
		if deployment != "" && deployment != info.DeploymentId {
			return nil, errors.New("test deployment identity changed")
		}
		deployment = info.DeploymentId
		switch info.Status {
		case "succeeded", "failed", "cancelled":
			return info, nil
		case "queued", "running":
		default:
			return nil, errors.New("test run has unknown status")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}
