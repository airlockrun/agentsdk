package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/sourcebundle"
)

type sourceFlags struct {
	url    string
	remote string
	agent  string
	force  bool
}

func cmdClone(args []string) error {
	f, positional, err := parseSourceFlags(args, false)
	if err != nil {
		return err
	}
	if f.agent != "" {
		return errors.New("clone takes its agent slug-or-id as the first positional argument, not --agent")
	}
	if len(positional) != 2 {
		return errors.New("clone requires an agent slug-or-id and destination directory")
	}
	baseURL, remoteName, err := resolveSourceAirlock(".", f, false)
	if err != nil {
		return err
	}
	ctx := context.Background()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	target, err := resolveAgentTarget(ctx, baseURL, token, positional[0], remoteName, agentRemoteBinding{})
	if err != nil {
		return err
	}
	dst := positional[1]
	_, statErr := os.Stat(dst)
	createdDst := os.IsNotExist(statErr)
	if statErr != nil && !createdDst {
		return statErr
	}
	if err := ensureEmptyDir(dst); err != nil {
		return err
	}
	cloneComplete := false
	if createdDst {
		defer func() {
			if !cloneComplete {
				_ = os.RemoveAll(dst)
			}
		}()
	}
	tmp, state, err := downloadSource(ctx, baseURL, token, target.AgentID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sourcebundle.Mirror(tmp, dst); err != nil {
		return fmt.Errorf("write cloned source: %w", err)
	}
	target.AirlockURL = baseURL
	target.SourceState = state
	binding := agentBinding{}
	binding.putRemote(remoteName, target)
	if err := writeAgentBinding(dst, binding); err != nil {
		return err
	}
	cloneComplete = true
	fmt.Printf("Cloned %s (%s) from %s into %s\n", target.Slug, target.AgentID, baseURL, dst)
	return nil
}

func cmdPull(args []string) error {
	f, positional, err := parseSourceFlags(args, true)
	if err != nil {
		return err
	}
	dir := "."
	if len(positional) > 1 {
		return errors.New("pull takes at most one positional argument: the agent repo directory")
	}
	if len(positional) == 1 {
		dir = positional[0]
	}
	binding, _, err := loadAgentBinding(dir)
	if err != nil {
		return err
	}
	baseURL, remoteName, err := resolveSourceAirlock(dir, f, true)
	if err != nil {
		return err
	}
	bound, _ := binding.remote(remoteName)
	ctx := context.Background()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	target, err := resolveAgentTarget(ctx, baseURL, token, f.agent, remoteName, bound)
	if err != nil {
		return err
	}
	tmp, remoteState, err := downloadSource(ctx, baseURL, token, target.AgentID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	localState, err := sourcebundle.Digest(dir)
	if err != nil {
		return fmt.Errorf("hash local source: %w", err)
	}
	if localState == remoteState {
		target.SourceState = remoteState
		target.AirlockURL = baseURL
		binding.putRemote(remoteName, target)
		if err := writeAgentBinding(dir, binding); err != nil {
			return err
		}
		fmt.Println("Local source already matches Airlock")
		return nil
	}
	baseState := target.SourceState
	if remoteState == baseState && !f.force {
		fmt.Println("Airlock source is unchanged; local source has unpushed changes")
		return nil
	}
	if !f.force && (baseState == "" || localState != baseState) {
		return sourceConflictError(target, baseURL, remoteName)
	}
	if err := sourcebundle.Mirror(tmp, dir); err != nil {
		return fmt.Errorf("update local source: %w", err)
	}
	state, err := sourcebundle.Digest(dir)
	if err != nil {
		return err
	}
	if state != remoteState {
		return fmt.Errorf("pulled source state %s, want %s", state, remoteState)
	}
	target.SourceState = remoteState
	target.AirlockURL = baseURL
	binding.putRemote(remoteName, target)
	if err := writeAgentBinding(dir, binding); err != nil {
		return err
	}
	fmt.Printf("Pulled %s (%s) from %s\n", target.Slug, target.AgentID, baseURL)
	return nil
}

func parseSourceFlags(args []string, allowForce bool) (sourceFlags, []string, error) {
	var f sourceFlags
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--force" {
			if !allowForce {
				return sourceFlags{}, nil, errors.New("clone does not support --force")
			}
			f.force = true
			continue
		}
		if !strings.HasPrefix(args[i], "--") {
			positional = append(positional, args[i])
			continue
		}
		if i+1 >= len(args) {
			return sourceFlags{}, nil, fmt.Errorf("flag %s needs a value", args[i])
		}
		key, value := strings.TrimPrefix(args[i], "--"), args[i+1]
		i++
		switch key {
		case "url":
			f.url = value
		case "remote":
			f.remote = value
		case "agent":
			f.agent = value
		default:
			return sourceFlags{}, nil, fmt.Errorf("unknown flag --%s", key)
		}
	}
	if f.remote != "" && !validRemoteName(f.remote) {
		return sourceFlags{}, nil, fmt.Errorf("invalid remote %q: use letters, digits, dashes, and underscores", f.remote)
	}
	return f, positional, nil
}

func resolveSourceAirlock(dir string, f sourceFlags, requireRemoteMatch bool) (string, string, error) {
	remoteName := f.remote
	if !requireRemoteMatch && f.url != "" {
		if remoteName == "" {
			remoteName = defaultRemoteName
		}
		return normalizeBaseURL(f.url), remoteName, nil
	}
	binding, _, err := loadAgentBinding(dir)
	if err != nil {
		return "", "", err
	}
	if remoteName == "" {
		remoteName = binding.DefaultRemote
	}
	if remoteName == "" {
		remoteName = defaultRemoteName
	}
	baseURL := normalizeBaseURL(f.url)
	if baseURL != "" && requireRemoteMatch {
		if remote, ok := binding.remote(remoteName); ok && remote.AirlockURL != "" && baseURL != normalizeBaseURL(remote.AirlockURL) {
			return "", "", fmt.Errorf("remote %q is bound to %s, not %s; choose a different --remote name", remoteName, remote.AirlockURL, baseURL)
		}
	}
	if baseURL == "" {
		if remote, ok := binding.remote(remoteName); ok {
			baseURL = remote.AirlockURL
		}
	}
	if baseURL == "" {
		creds, err := loadCredentials()
		if err != nil {
			return "", "", err
		}
		if len(creds.Sessions) == 1 {
			for url := range creds.Sessions {
				baseURL = url
			}
		} else if len(creds.Sessions) == 0 {
			return "", "", errors.New("source command needs an Airlock URL: pass --url after running go tool air login")
		} else {
			var urls []string
			for url := range creds.Sessions {
				urls = append(urls, url)
			}
			sort.Strings(urls)
			return "", "", fmt.Errorf("multiple Airlock logins found; pass --url with one of: %s", strings.Join(urls, ", "))
		}
	}
	return baseURL, remoteName, nil
}

func downloadSource(ctx context.Context, baseURL, token, agentID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeBaseURL(baseURL)+"/api/v1/agents/"+agentID+"/source", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := apiClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var er airlockv1.ErrorResponse
		if err := protoUnmarshal.Unmarshal(body, &er); err == nil && er.Error != "" {
			return "", "", fmt.Errorf("download source: %s: %s", resp.Status, er.Error)
		}
		return "", "", fmt.Errorf("download source: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	state := unquoteETag(resp.Header.Get("ETag"))
	if state == "" {
		return "", "", errors.New("download source: Airlock response did not include ETag")
	}
	tmp, err := os.MkdirTemp("", "air-source-*")
	if err != nil {
		return "", "", err
	}
	got, err := sourcebundle.ExtractArchiveState(resp.Body, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return "", "", err
	}
	if got != state {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("downloaded source state %s, response declared %s", got, state)
	}
	return tmp, state, nil
}

func sourceConflictError(target agentRemoteBinding, baseURL, remoteName string) error {
	return fmt.Errorf("local and Airlock source both changed since the last sync.\n\nClone the current source into another directory:\n  air clone %s ../%s-airlock --remote %s --url %s\n\nMerge your changes into that directory, then deploy from there", target.AgentID, target.Slug, remoteName, baseURL)
}

func quoteETag(state string) string {
	return strconv.Quote(strings.TrimSpace(state))
}

func unquoteETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}
