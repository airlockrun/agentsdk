package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/internal/bootstrap"
	"github.com/airlockrun/agentsdk/sourcebundle"
	"github.com/airlockrun/agentsdk/wire"
)

type sourceMetadata struct {
	ETag       string
	State      string
	Revision   string
	Generation string
}

type sourceFlags struct {
	url    string
	remote string
	agent  string
	force  bool
}

func cmdClone(args []string) (retErr error) {
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
	isBootstrap, err := bootstrap.EnsureDir(dst)
	if err != nil {
		return err
	}
	var bootstrapFiles map[string][]byte
	if isBootstrap {
		bootstrapFiles, err = snapshotBootstrapFiles(dst)
		if err != nil {
			return err
		}
	}
	cloneComplete := false
	defer func() {
		if cloneComplete {
			return
		}
		var cleanupErr error
		switch {
		case isBootstrap:
			cleanupErr = restoreBootstrapFiles(dst, bootstrapFiles)
		case createdDst:
			cleanupErr = os.RemoveAll(dst)
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore clone destination: %w", cleanupErr))
		}
	}()
	tmp, metadata, err := downloadSource(ctx, baseURL, token, target.AgentID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sourcebundle.Mirror(tmp, dst); err != nil {
		return fmt.Errorf("write cloned source: %w", err)
	}
	target.AirlockURL = baseURL
	target.SourceETag = metadata.ETag
	target.SourceState = metadata.State
	binding := agentBinding{}
	binding.putRemote(remoteName, target)
	if err := writeAgentBinding(dst, binding); err != nil {
		return err
	}
	cloneComplete = true
	fmt.Printf("Cloned %s (%s) from %s into %s\n", target.Slug, target.AgentID, baseURL, dst)
	return nil
}

func snapshotBootstrapFiles(dir string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	for _, name := range []string{"go.mod", "go.sum"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) && name == "go.sum" {
				continue
			}
			return nil, fmt.Errorf("read bootstrap %s: %w", name, err)
		}
		files[name] = body
	}
	return files, nil
}

func restoreBootstrapFiles(dir string, files map[string][]byte) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return err
		}
	}
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
	tmp, metadata, err := downloadSource(ctx, baseURL, token, target.AgentID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	remoteState := metadata.State
	localState, err := sourcebundle.Digest(dir)
	if err != nil {
		return fmt.Errorf("hash local source: %w", err)
	}
	if localState == remoteState {
		target.SourceState = remoteState
		target.SourceETag = metadata.ETag
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
	target.SourceETag = metadata.ETag
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

func downloadSource(ctx context.Context, baseURL, token, agentID string) (string, sourceMetadata, error) {
	resp, err := doAuthenticatedHTTP(ctx, baseURL, token, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizeBaseURL(baseURL)+"/api/v1/agents/"+agentID+"/source", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return "", sourceMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		var er airlockv1.ErrorResponse
		if err := protoUnmarshal.Unmarshal(body, &er); err == nil && er.Error != "" {
			return "", sourceMetadata{}, fmt.Errorf("download source: %s: %s", resp.Status, er.Error)
		}
		return "", sourceMetadata{}, fmt.Errorf("download source: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	metadata := sourceMetadata{
		ETag:       unquoteETag(resp.Header.Get("ETag")),
		State:      strings.TrimSpace(resp.Header.Get(wire.SourceStateHeader)),
		Revision:   strings.TrimSpace(resp.Header.Get(wire.SourceRevisionHeader)),
		Generation: strings.TrimSpace(resp.Header.Get(wire.SourceGenerationHeader)),
	}
	version, err := wire.ParseSourceETag(metadata.ETag)
	if err != nil {
		return "", sourceMetadata{}, fmt.Errorf("download source: %w", err)
	}
	if metadata.State == "" || metadata.Revision == "" || metadata.Generation == "" {
		return "", sourceMetadata{}, errors.New("download source: Airlock response omitted source revision metadata")
	}
	if metadata.Revision != version.Revision || metadata.Generation != strconv.FormatInt(version.Generation, 10) {
		return "", sourceMetadata{}, errors.New("download source: Airlock response contained inconsistent source revision metadata")
	}
	tmp, err := os.MkdirTemp("", "air-source-*")
	if err != nil {
		return "", sourceMetadata{}, err
	}
	got, err := sourcebundle.ExtractArchiveState(resp.Body, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return "", sourceMetadata{}, err
	}
	if got != metadata.State {
		os.RemoveAll(tmp)
		return "", sourceMetadata{}, fmt.Errorf("downloaded source state %s, response declared %s", got, metadata.State)
	}
	return tmp, metadata, nil
}

func sourceConflictError(target agentRemoteBinding, baseURL, remoteName string) error {
	return fmt.Errorf("local and Airlock source both changed since the last sync.\n\nClone the current source into another directory:\n  airlock clone %s ../%s-airlock --remote %s --url %s\n\nMerge your changes into that directory, then deploy from there", target.AgentID, target.Slug, remoteName, baseURL)
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
