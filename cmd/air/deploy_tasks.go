package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

type deploymentTask struct {
	Info    *airlockv1.TaskResponse
	Error   string
	BuildID string
}

func getDeploymentTask(ctx context.Context, baseURL, token, taskID string) (deploymentTask, error) {
	if !deployUUIDRe.MatchString(taskID) {
		return deploymentTask{}, errors.New("saved deployment task is not a UUID")
	}
	response, err := doAuthenticatedHTTP(ctx, baseURL, token, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeBaseURL(baseURL)+"/api/v1/tasks/"+url.PathEscape(taskID), nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return deploymentTask{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return deploymentTask{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return deploymentTask{}, newHTTPStatusError(response.StatusCode, response.Status, body)
	}
	var info airlockv1.TaskResponse
	if err := protoUnmarshal.Unmarshal(body, &info); err != nil {
		return deploymentTask{}, err
	}
	if strings.TrimPrefix(info.Id, "task:") != taskID {
		return deploymentTask{}, errors.New("deployment task response selected a different task")
	}
	return deploymentTask{Info: &info, Error: response.Header.Get("X-Airlock-Task-Error"), BuildID: response.Header.Get("X-Airlock-Build-ID")}, nil
}
