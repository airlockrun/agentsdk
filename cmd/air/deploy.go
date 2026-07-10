package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/airlockrun/agentsdk"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/scaffold"
	"github.com/airlockrun/agentsdk/sourcebundle"
)

var (
	deployUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	agentSlugRe  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type deployFlags struct {
	create      bool
	force       bool
	slug        string
	agent       string
	url         string
	remote      string
	name        string
	description string
	dir         string
}

func cmdDeploy(args []string) error {
	f, err := parseDeployFlags(args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(f.dir, "go.mod")); err != nil {
		return fmt.Errorf("deploy requires an agent repo with go.mod in %s: %w", f.dir, err)
	}
	binding, _, err := loadAgentBinding(f.dir)
	if err != nil {
		return err
	}
	remoteName := f.remote
	if remoteName == "" {
		remoteName = binding.DefaultRemote
	}
	if remoteName == "" {
		remoteName = defaultRemoteName
	}
	boundRemote, _ := binding.remote(remoteName)
	baseURL := normalizeBaseURL(f.url)
	if baseURL == "" {
		baseURL = boundRemote.AirlockURL
	}
	if baseURL == "" {
		return errors.New("deploy needs an Airlock URL: pass --url or run air init --airlock <url>")
	}

	ctx := context.Background()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	if err := ensureDeploySDKVersion(ctx, baseURL, token); err != nil {
		return err
	}

	before, err := snapshotManagedFiles(f.dir)
	if err != nil {
		return err
	}
	if err := runUpdate(f.dir, scaffold.ScaffoldData{
		AgentSDKVersion: "v" + agentsdk.Version,
		AgentBaseImage:  defaultBaseImage,
	}); err != nil {
		return err
	}
	after, err := snapshotManagedFiles(f.dir)
	if err != nil {
		return err
	}
	if changed := changedManagedFiles(before, after); len(changed) > 0 {
		return fmt.Errorf("update changed airlock-managed files (%s); review the changes and rerun deploy", strings.Join(changed, ", "))
	}

	var target agentRemoteBinding
	if f.create {
		target, err = createDraftAgent(ctx, baseURL, token, f)
		if err != nil {
			return err
		}
		target.AirlockURL = baseURL
		binding.setRemote(remoteName, target)
		if err := writeAgentBinding(f.dir, binding); err != nil {
			return err
		}
	} else {
		target, err = resolveDeployTarget(ctx, baseURL, token, f.agent, boundRemote)
		if err != nil {
			return err
		}
	}

	fmt.Printf("Deploying %s to %s (%s) at %s\n", f.dir, target.Slug, target.AgentID, baseURL)
	localState, err := sourcebundle.Digest(f.dir)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	previousState := target.SourceState
	newState, err := uploadSource(ctx, baseURL, token, target.AgentID, f.dir, previousState, f.force)
	if err != nil {
		var stale *staleSourceError
		if errors.As(err, &stale) {
			if stale.gitRemote != "" {
				branchArg := ""
				if stale.gitBranch != "" {
					branchArg = " --branch " + stale.gitBranch
				}
				return fmt.Errorf("the connected Git branch changed since this workspace last synced.\n\nClone the current branch into another directory:\n  git clone%s %s ../%s-latest\n\nMerge your changes there and push through Git", branchArg, stale.gitRemote, target.Slug)
			}
			return fmt.Errorf("Airlock source changed since this workspace last synced.\n\nClone the current source into another directory:\n  air clone %s ../%s-airlock --url %s\n\nMerge your changes into that directory, then deploy from there:\n  cd ../%s-airlock\n  air deploy\n\nUse --force only to replace Airlock's current source", target.AgentID, target.Slug, baseURL, target.Slug)
		}
		return err
	}
	if newState != localState {
		return fmt.Errorf("Airlock returned source state %s, want uploaded state %s", newState, localState)
	}
	target.SourceState = newState
	target.AirlockURL = baseURL
	binding.setRemote(remoteName, target)
	if err := writeAgentBinding(f.dir, binding); err != nil {
		return err
	}
	if previousState == newState && !f.force {
		fmt.Println("Source is unchanged; no build started")
	} else {
		fmt.Println("Source uploaded; build started")
	}
	return nil
}

func parseDeployFlags(args []string) (deployFlags, error) {
	f := deployFlags{dir: "."}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[:2] != "--" {
			positional = append(positional, a)
			continue
		}
		key := a[2:]
		if key == "create" {
			f.create = true
			continue
		}
		if key == "force" {
			f.force = true
			continue
		}
		if i+1 >= len(args) {
			return deployFlags{}, fmt.Errorf("flag --%s needs a value", key)
		}
		i++
		value := args[i]
		switch key {
		case "slug":
			f.slug = value
		case "agent":
			f.agent = value
		case "url":
			f.url = value
		case "remote":
			f.remote = value
		case "name":
			f.name = value
		case "description":
			f.description = value
		default:
			return deployFlags{}, fmt.Errorf("unknown flag --%s", key)
		}
	}
	switch len(positional) {
	case 0:
	case 1:
		f.dir = positional[0]
	default:
		return deployFlags{}, errors.New("deploy takes at most one positional argument: the agent repo directory")
	}
	if f.create && f.agent != "" {
		return deployFlags{}, errors.New("deploy cannot combine --create and --agent")
	}
	if f.remote != "" && !validRemoteName(f.remote) {
		return deployFlags{}, fmt.Errorf("invalid remote %q: use letters, digits, dashes, and underscores", f.remote)
	}
	if f.create {
		if f.name == "" {
			if f.slug != "" {
				f.name = f.slug
			} else {
				f.name = filepath.Base(filepath.Clean(f.dir))
			}
		}
		if f.slug == "" {
			f.slug = slugFromName(f.name)
			if f.slug == "" {
				return deployFlags{}, fmt.Errorf("could not derive a slug from name %q; pass --slug", f.name)
			}
		}
		if !validAgentSlug(f.slug) {
			return deployFlags{}, fmt.Errorf("invalid slug %q: use 2-63 lowercase letters, digits, and single dashes", f.slug)
		}
	}
	return f, nil
}

func ensureDeploySDKVersion(ctx context.Context, baseURL, token string) error {
	var resp airlockv1.GetAgentSDKInfoResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agent-sdk", token, nil, &resp); err != nil {
		return fmt.Errorf("check Airlock SDK version: %w", err)
	}
	serverVersion := strings.TrimPrefix(resp.GetVersion(), "v")
	localVersion := strings.TrimPrefix(agentsdk.Version, "v")
	if serverVersion == "" {
		return errors.New("check Airlock SDK version: server response did not include a version")
	}
	if serverVersion == localVersion {
		return nil
	}
	commandImport := resp.GetCommandImport()
	if commandImport == "" {
		commandImport = "github.com/airlockrun/agentsdk/cmd/air"
	}
	return fmt.Errorf("Airlock uses agentsdk v%s, but this air CLI is v%s; update this repo, validate the build, then rerun deploy:\n  go get github.com/airlockrun/agentsdk@v%s\n  go get -tool %s@v%s\n  go mod tidy\n  go tool air build", serverVersion, localVersion, serverVersion, commandImport, serverVersion)
}

func slugFromName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 63 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) < 2 || !validAgentSlug(out) {
		return ""
	}
	return out
}

func validAgentSlug(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	return agentSlugRe.MatchString(s)
}

func createDraftAgent(ctx context.Context, baseURL, token string, f deployFlags) (agentRemoteBinding, error) {
	name := f.name
	if name == "" {
		name = f.slug
	}
	var resp airlockv1.CreateAgentResponse
	err := doProto(ctx, baseURL, http.MethodPost, "/api/v1/agents", token, &airlockv1.CreateAgentRequest{
		Name:             name,
		Slug:             f.slug,
		Description:      f.description,
		SkipInitialBuild: true,
	}, &resp)
	if err != nil {
		return agentRemoteBinding{}, fmt.Errorf("create agent: %w", err)
	}
	if resp.Agent == nil || resp.Agent.Id == "" {
		return agentRemoteBinding{}, errors.New("create agent response did not include an agent id")
	}
	return agentRemoteBinding{AirlockURL: baseURL, AgentID: resp.Agent.Id, Slug: resp.Agent.Slug}, nil
}

func resolveDeployTarget(ctx context.Context, baseURL, token, flagAgent string, binding agentRemoteBinding) (agentRemoteBinding, error) {
	target := flagAgent
	if target == "" {
		target = binding.AgentID
	}
	if target == "" && binding.Slug != "" {
		return agentRemoteBinding{}, fmt.Errorf("%s has slug %q but no agent_id; run air deploy --agent %s once to resolve and persist the stable binding", agentBindingPath, binding.Slug, binding.Slug)
	}
	if target == "" {
		return agentRemoteBinding{}, fmt.Errorf("deploy needs a target: pass --agent, --create, or set %s", agentBindingPath)
	}
	if deployUUIDRe.MatchString(target) {
		agent, err := getAgentDetail(ctx, baseURL, token, target)
		if err != nil {
			return agentRemoteBinding{}, err
		}
		if binding.AgentID == target && binding.Slug != "" && agent.GetSlug() != binding.Slug {
			return agentRemoteBinding{}, fmt.Errorf("%s has agent_id %s but slug %q; Airlock reports slug %q", agentBindingPath, target, binding.Slug, agent.GetSlug())
		}
		return agentRemoteBinding{AirlockURL: baseURL, AgentID: target, Slug: agent.GetSlug(), SourceState: binding.SourceState}, nil
	}

	var resp airlockv1.ListAgentsResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents", token, nil, &resp); err != nil {
		return agentRemoteBinding{}, fmt.Errorf("resolve agent %q: %w", target, err)
	}
	for _, a := range resp.Agents {
		if a.GetSlug() == target || a.GetId() == target {
			if binding.AgentID != "" && binding.Slug == target && a.GetId() != binding.AgentID {
				return agentRemoteBinding{}, fmt.Errorf("%s has slug %q but agent_id %s; Airlock reports agent_id %s", agentBindingPath, binding.Slug, binding.AgentID, a.GetId())
			}
			return agentRemoteBinding{AirlockURL: baseURL, AgentID: a.GetId(), Slug: a.GetSlug(), SourceState: binding.SourceState}, nil
		}
	}
	return agentRemoteBinding{}, fmt.Errorf("agent %q not found in %s", target, baseURL)
}

func getAgentDetail(ctx context.Context, baseURL, token, agentID string) (*airlockv1.AgentInfo, error) {
	var resp airlockv1.GetAgentDetailResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents/"+agentID, token, nil, &resp); err != nil {
		return nil, fmt.Errorf("resolve agent %q: %w", agentID, err)
	}
	if resp.Agent == nil || resp.Agent.Id == "" {
		return nil, fmt.Errorf("resolve agent %q: response did not include agent detail", agentID)
	}
	return resp.Agent, nil
}

type staleSourceError struct {
	gitRemote string
	gitBranch string
}

func (e *staleSourceError) Error() string { return "source state is stale" }

func uploadSource(ctx context.Context, baseURL, token, agentID, dir, sourceState string, force bool) (string, error) {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeSourceArchive(pw, dir))
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, normalizeBaseURL(baseURL)+"/api/v1/agents/"+agentID+"/source", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	if sourceState != "" {
		req.Header.Set("If-Match", quoteETag(sourceState))
	}
	if force {
		req.Header.Set("X-Airlock-Force", "true")
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusPreconditionRequired {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &staleSourceError{
			gitRemote: strings.TrimSpace(resp.Header.Get("X-Airlock-Git-Remote")),
			gitBranch: strings.TrimSpace(resp.Header.Get("X-Airlock-Git-Branch")),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		var er airlockv1.ErrorResponse
		if err := protoUnmarshal.Unmarshal(b, &er); err == nil && er.Error != "" {
			return "", fmt.Errorf("upload source: %s: %s", resp.Status, er.Error)
		}
		return "", fmt.Errorf("upload source: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	state := unquoteETag(resp.Header.Get("ETag"))
	if state == "" {
		return "", errors.New("upload source: Airlock response did not include ETag")
	}
	return state, nil
}

func writeSourceArchive(w io.Writer, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("source archive requires go.mod at repo root: %w", err)
	}
	_, err := sourcebundle.WriteArchive(w, dir)
	return err
}

func snapshotManagedFiles(dir string) (map[string][]byte, error) {
	files := []string{"Dockerfile", "AGENTS.md", scaffold.NoticesFilename}
	out := make(map[string][]byte, len(files))
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		out[file] = b
	}
	return out, nil
}

func changedManagedFiles(before, after map[string][]byte) []string {
	var changed []string
	for name, b := range before {
		if string(b) != string(after[name]) {
			changed = append(changed, name)
		}
	}
	return changed
}
