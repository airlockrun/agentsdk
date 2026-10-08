package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
)

type deploymentTask struct {
	Info    *airlockv1.TaskResponse
	Error   string
	BuildID string
}

func hydrateDeveloperSource(ctx context.Context, baseURL, token string, remote *agentRemoteBinding) error {
	if remote.AgentID == "" || remote.SourceETag != "" || remote.PendingTask != "" {
		return nil
	}
	metadata, err := observeSourceMetadata(ctx, baseURL, token, remote.AgentID)
	if hasHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("observe deployed source for developer workspace: %w", err)
	}
	remote.SourceState, remote.SourceETag = metadata.State, metadata.ETag
	return nil
}

func getDeploymentTask(ctx context.Context, baseURL, token, taskID string) (deploymentTask, error) {
	if !deployUUIDRe.MatchString(taskID) {
		return deploymentTask{}, errors.New("saved deployment task is not a UUID")
	}
	resp, err := doAuthenticatedHTTP(ctx, baseURL, token, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeBaseURL(baseURL)+"/api/v1/tasks/"+url.PathEscape(taskID), nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return deploymentTask{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return deploymentTask{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deploymentTask{}, newHTTPStatusError(resp.StatusCode, resp.Status, body)
	}
	var info airlockv1.TaskResponse
	if err := protoUnmarshal.Unmarshal(body, &info); err != nil {
		return deploymentTask{}, err
	}
	task := deploymentTask{Info: &info, Error: resp.Header.Get("X-Airlock-Task-Error"), BuildID: resp.Header.Get("X-Airlock-Build-ID")}
	return task, nil
}

func reconcilePendingDeployment(ctx context.Context, dir, remoteName string, binding *agentBinding, remote *agentRemoteBinding, baseURL, token string) error {
	if remote.PendingTask == "" {
		return nil
	}
	task, err := getDeploymentTask(ctx, baseURL, token, remote.PendingTask)
	if err != nil {
		return fmt.Errorf("check previous deployment task %s: %w", remote.PendingTask, err)
	}
	switch task.Info.Status {
	case "queued", "running":
		return fmt.Errorf("deployment task %s is still %s; run go tool air status", remote.PendingTask, task.Info.Status)
	case "failed", "cancelled":
		remote.PendingTask, remote.PendingSourceState = "", ""
		binding.putRemote(remoteName, *remote)
		if writeErr := writeAgentBinding(dir, *binding); writeErr != nil {
			return writeErr
		}
		if task.Error == "" {
			task.Error = task.Info.Status
		}
		return fmt.Errorf("previous deployment task %s %s: %s", strings.TrimPrefix(task.Info.Id, "task:"), task.Info.Status, task.Error)
	case "succeeded":
		metadata, err := observeSourceMetadata(ctx, baseURL, token, remote.AgentID)
		if err != nil {
			return fmt.Errorf("refresh deployed source after task %s: %w", remote.PendingTask, err)
		}
		if metadata.State != remote.PendingSourceState {
			return fmt.Errorf("deployment task %s succeeded but canonical source is %s, want accepted candidate %s", remote.PendingTask, metadata.State, remote.PendingSourceState)
		}
		remote.SourceState, remote.SourceETag = metadata.State, metadata.ETag
		remote.PendingTask, remote.PendingSourceState = "", ""
		binding.putRemote(remoteName, *remote)
		return writeAgentBinding(dir, *binding)
	default:
		return fmt.Errorf("deployment task %s has unknown status %q", remote.PendingTask, task.Info.Status)
	}
}

func observeSourceMetadata(ctx context.Context, baseURL, token, agentID string) (sourceMetadata, error) {
	resp, err := doAuthenticatedHTTP(ctx, baseURL, token, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, normalizeBaseURL(baseURL)+"/api/v1/agents/"+url.PathEscape(agentID)+"/source", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return sourceMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return sourceMetadata{}, newHTTPStatusError(resp.StatusCode, resp.Status, body)
	}
	metadata := sourceMetadata{ETag: unquoteETag(resp.Header.Get("ETag")), State: strings.TrimSpace(resp.Header.Get(wire.SourceStateHeader))}
	if _, err := wire.ParseSourceETag(metadata.ETag); err != nil {
		return sourceMetadata{}, err
	}
	if metadata.State == "" {
		return sourceMetadata{}, errors.New("source response omitted source state")
	}
	return metadata, nil
}
