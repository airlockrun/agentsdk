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
	"golang.org/x/mod/semver"
)

var (
	deployUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	agentSlugRe  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

const maxDeployMessageBytes = 200

type deployFlags struct {
	create      bool
	force       bool
	slug        string
	agent       string
	url         string
	remote      string
	name        string
	description string
	message     string
	dir         string
}

func cmdDeploy(args []string) error {
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return errors.New("deploy is unavailable with a codegen integration token")
	}
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
	if baseURL != "" && boundRemote.AirlockURL != "" && baseURL != normalizeBaseURL(boundRemote.AirlockURL) {
		return fmt.Errorf("remote %q is bound to %s, not %s; choose a different --remote name", remoteName, boundRemote.AirlockURL, baseURL)
	}
	if baseURL == "" {
		baseURL = boundRemote.AirlockURL
	}
	if baseURL == "" {
		return errors.New("deploy needs an Airlock URL: pass --url or configure an Airlock remote for this workspace")
	}
	if f.create && boundRemote.AgentID != "" {
		return fmt.Errorf("remote %q is already bound to %s (%s); deploy without --create to update that agent\n\nTo intentionally replace this local binding, run from %s:\n  go tool air remote unbind %s", remoteName, boundRemote.Slug, boundRemote.AgentID, f.dir, remoteName)
	}

	ctx := context.Background()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	if err := ensureDeploySDKVersion(ctx, baseURL, token); err != nil {
		return err
	}
	var target agentRemoteBinding
	if !f.create {
		target, err = resolveAgentTarget(ctx, baseURL, token, f.agent, remoteName, boundRemote)
		if err != nil {
			return explainDeployTargetError(ctx, baseURL, token, remoteName, f.dir, f.agent, boundRemote, err)
		}
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

	if f.create {
		target, err = createDraftAgent(ctx, baseURL, token, f)
		if err != nil {
			return explainCreateAgentError(ctx, baseURL, token, remoteName, f.dir, f.slug, err)
		}
		target.AirlockURL = baseURL
		binding.putRemote(remoteName, target)
		if err := writeAgentBinding(f.dir, binding); err != nil {
			return err
		}
	}

	fmt.Printf("Deploying %s to %s (%s) at %s\n", f.dir, target.Slug, target.AgentID, baseURL)
	localState, err := sourcebundle.Digest(f.dir)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	previousState := target.SourceState
	newState, err := uploadSource(ctx, baseURL, token, target.AgentID, f.dir, previousState, f.message, f.force)
	if err != nil {
		var stale *staleSourceError
		if errors.As(err, &stale) {
			return deploySourceStateError(stale, target, baseURL, remoteName)
		}
		bindingStored := f.create || boundRemote.AgentID == target.AgentID
		if hasHTTPStatus(err, http.StatusNotFound) && bindingStored {
			return missingBoundAgentError(ctx, baseURL, token, remoteName, f.dir, target, f.create)
		}
		if hasHTTPStatus(err, http.StatusForbidden) {
			return fmt.Errorf("Airlock refused source deployment to %s (%s): the current login does not have permission to deploy this agent.\n\nNo source was uploaded. Ask an agent administrator for admin access, or use a different remote", target.Slug, target.AgentID)
		}
		if hasHTTPStatus(err, http.StatusUnauthorized) {
			return fmt.Errorf("Airlock rejected the login while deploying to %s (%s).\n\nLog in again, then rerun deploy:\n  go tool air login %s --reauthenticate", target.Slug, target.AgentID, baseURL)
		}
		if hasHTTPStatus(err, http.StatusConflict) {
			return fmt.Errorf("Airlock refused source deployment to %s (%s): %w", target.Slug, target.AgentID, err)
		}
		return err
	}
	if newState != localState {
		return fmt.Errorf("Airlock returned source state %s, want uploaded state %s", newState, localState)
	}
	target.SourceState = newState
	target.AirlockURL = baseURL
	binding.putRemote(remoteName, target)
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
	messageSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-m" {
			if i+1 >= len(args) {
				return deployFlags{}, errors.New("flag -m needs a value")
			}
			i++
			f.message = args[i]
			messageSet = true
			continue
		}
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
		case "message":
			f.message = value
			messageSet = true
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
	if !f.create && (f.slug != "" || f.name != "" || f.description != "") {
		return deployFlags{}, errors.New("deploy flags --slug, --name, and --description require --create")
	}
	if f.remote != "" && !validRemoteName(f.remote) {
		return deployFlags{}, fmt.Errorf("invalid remote %q: use letters, digits, dashes, and underscores", f.remote)
	}
	if !messageSet {
		return deployFlags{}, errors.New("deploy requires -m or --message")
	}
	f.message = strings.TrimSpace(f.message)
	if f.message == "" {
		return deployFlags{}, errors.New("deploy message must not be blank")
	}
	if strings.ContainsAny(f.message, "\r\n") {
		return deployFlags{}, errors.New("deploy message must be a single line")
	}
	if len(f.message) > maxDeployMessageBytes {
		return deployFlags{}, fmt.Errorf("deploy message is %d bytes; maximum is %d", len(f.message), maxDeployMessageBytes)
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
	if compatibleSDKVersions(serverVersion, localVersion) {
		return nil
	}
	commandImport := resp.GetCommandImport()
	if commandImport == "" {
		commandImport = "github.com/airlockrun/agentsdk/cmd/air"
	}
	return fmt.Errorf("Airlock uses agentsdk v%s, but this air CLI is v%s; update this repo, validate the build, then rerun deploy:\n  go get -tool %s@v%s\n  go tool air update\n  go tool air build", serverVersion, localVersion, commandImport, serverVersion)
}

// compatibleSDKVersions accepts a local CLI from the same major/minor series
// when it is at least as new as the SDK pinned by Airlock. This lets one current
// workspace deploy to multiple slightly older remotes without allowing an old
// CLI to drive a newer Airlock deployment protocol.
func compatibleSDKVersions(server, local string) bool {
	server = "v" + strings.TrimPrefix(server, "v")
	local = "v" + strings.TrimPrefix(local, "v")
	if !semver.IsValid(server) || !semver.IsValid(local) {
		return false
	}
	if semver.MajorMinor(server) != semver.MajorMinor(local) {
		return false
	}
	return semver.Compare(local, server) >= 0
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

func resolveAgentTarget(ctx context.Context, baseURL, token, flagAgent, remoteName string, binding agentRemoteBinding) (agentRemoteBinding, error) {
	target := flagAgent
	if target == "" {
		target = binding.AgentID
	}
	if target == "" {
		return agentRemoteBinding{}, fmt.Errorf("remote %q needs an agent target: pass --agent or configure %s", remoteName, agentBindingPath)
	}
	var resolved agentRemoteBinding
	if deployUUIDRe.MatchString(target) {
		agent, err := getAgentDetail(ctx, baseURL, token, target)
		if err != nil {
			return agentRemoteBinding{}, err
		}
		if agent.GetId() != target {
			return agentRemoteBinding{}, fmt.Errorf("resolve agent %q: Airlock returned agent %q", target, agent.GetId())
		}
		resolved = agentRemoteBinding{AirlockURL: baseURL, AgentID: agent.GetId(), Slug: agent.GetSlug()}
	} else {
		var resp airlockv1.ListAgentsResponse
		if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents", token, nil, &resp); err != nil {
			return agentRemoteBinding{}, fmt.Errorf("resolve agent %q: %w", target, err)
		}
		for _, a := range resp.Agents {
			if a.GetSlug() == target || a.GetId() == target {
				resolved = agentRemoteBinding{AirlockURL: baseURL, AgentID: a.GetId(), Slug: a.GetSlug()}
				break
			}
		}
	}
	if resolved.AgentID == "" {
		return agentRemoteBinding{}, fmt.Errorf("agent %q not found in %s", target, baseURL)
	}
	if binding.AgentID != "" && resolved.AgentID != binding.AgentID {
		return agentRemoteBinding{}, &agentBindingMismatchError{
			remoteName:   remoteName,
			boundID:      binding.AgentID,
			boundSlug:    binding.Slug,
			resolvedID:   resolved.AgentID,
			resolvedSlug: resolved.Slug,
		}
	}
	if normalizeBaseURL(binding.AirlockURL) == normalizeBaseURL(baseURL) && binding.AgentID == resolved.AgentID {
		resolved.SourceState = binding.SourceState
	}
	return resolved, nil
}

type agentBindingMismatchError struct {
	remoteName   string
	boundID      string
	boundSlug    string
	resolvedID   string
	resolvedSlug string
}

func (e *agentBindingMismatchError) Error() string {
	return fmt.Sprintf("remote %q is bound to agent %s, not %s; choose a different --remote name", e.remoteName, e.boundID, e.resolvedID)
}

func explainDeployTargetError(ctx context.Context, baseURL, token, remoteName, dir, requested string, binding agentRemoteBinding, err error) error {
	var mismatch *agentBindingMismatchError
	if errors.As(err, &mismatch) {
		return fmt.Errorf("remote %q is bound to %s (%s), but --agent resolved to %s (%s).\n\nNo source was uploaded, and this workspace was not rebound. To target the different agent intentionally, run from %s:\n  go tool air remote unbind %s\n  go tool air deploy --remote %s --agent %s -m \"Describe this deployment\"\n\nOr keep this binding and choose a different --remote name", remoteName, mismatch.boundSlug, mismatch.boundID, mismatch.resolvedSlug, mismatch.resolvedID, dir, remoteName, remoteName, mismatch.resolvedID)
	}

	usesBoundID := binding.AgentID != "" && (requested == "" || requested == binding.AgentID)
	if hasHTTPStatus(err, http.StatusNotFound) {
		if usesBoundID {
			return missingBoundAgentError(ctx, baseURL, token, remoteName, dir, binding, false)
		}
		return fmt.Errorf("agent %q does not exist in %s; no source was uploaded", requested, baseURL)
	}
	if hasHTTPStatus(err, http.StatusForbidden) {
		if usesBoundID {
			return fmt.Errorf("the agent bound to remote %q still exists, but the current login cannot access it.\n\n  Agent: %s (%s)\n  Airlock: %s\n\nNo source was uploaded, and this workspace was not rebound. Log in with the correct account or ask an agent administrator to restore access", remoteName, binding.Slug, binding.AgentID, baseURL)
		}
		return fmt.Errorf("the current login cannot access agent %q in %s; no source was uploaded", requested, baseURL)
	}
	if binding.AgentID != "" && requested == binding.Slug && strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("agent slug %q was not found, but remote %q remains bound to stable agent ID %s.\n\nOmit --agent to deploy to the bound agent; Airlock will refresh its current slug", requested, remoteName, binding.AgentID)
	}
	return err
}

func explainCreateAgentError(ctx context.Context, baseURL, token, remoteName, dir, slug string, err error) error {
	if !hasHTTPStatus(err, http.StatusConflict) {
		return err
	}
	agent, lookupErr := visibleAgentBySlug(ctx, baseURL, token, slug)
	if lookupErr != nil {
		return fmt.Errorf("Airlock reports that agent slug %q is already in use. No agent was created.\n\nAirlock could not determine whether the existing agent is visible to this account: %v\nChoose a different --slug or ask an Airlock administrator to identify the existing agent", slug, lookupErr)
	}
	if agent == nil {
		return fmt.Errorf("Airlock reports that agent slug %q is already in use, but no accessible agent with that slug is visible to this account.\n\nNo agent was created. Choose a different --slug or ask an Airlock administrator for access", slug)
	}
	return fmt.Errorf("Airlock already has an accessible agent using slug %q (%s). No agent was created.\n\nTo deploy this workspace to that agent intentionally, run from %s:\n  go tool air deploy --remote %s --url %s --agent %s -m \"Describe this deployment\"\n\nOtherwise choose a different --slug", slug, agent.GetId(), dir, remoteName, baseURL, agent.GetId())
}

func missingBoundAgentError(ctx context.Context, baseURL, token, remoteName, dir string, binding agentRemoteBinding, newlyBound bool) error {
	replacement, lookupErr := visibleAgentBySlug(ctx, baseURL, token, binding.Slug)
	var detail string
	var nextDeploy string
	switch {
	case lookupErr != nil:
		detail = fmt.Sprintf("\n\nAirlock could not check whether another agent uses the saved slug %q: %v", binding.Slug, lookupErr)
	case replacement != nil && replacement.GetId() != binding.AgentID:
		detail = fmt.Sprintf("\n\nA different accessible agent now uses slug %q:\n  Bound agent ID: %s\n  Current agent ID: %s\n\nThe CLI will not deploy to the different agent automatically", binding.Slug, binding.AgentID, replacement.GetId())
		nextDeploy = fmt.Sprintf("\n\nThen deploy to the different agent intentionally:\n  go tool air deploy --remote %s --agent %s -m \"Describe this deployment\"\n\nOr create a new agent with a different slug:\n  go tool air deploy --remote %s --create --slug <new-slug> -m \"Describe this deployment\"", remoteName, replacement.GetId(), remoteName)
	case binding.Slug != "":
		detail = fmt.Sprintf("\n\nNo different accessible agent using the saved slug %q was found. The slug may still be unavailable to this account", binding.Slug)
	}
	createSlug := binding.Slug
	if createSlug == "" {
		createSlug = "<slug>"
	}
	if nextDeploy == "" {
		nextDeploy = fmt.Sprintf("\n\nThen create a new agent:\n  go tool air deploy --remote %s --create --slug %s -m \"Describe this deployment\"\n\nOr bind to an existing agent:\n  go tool air deploy --remote %s --agent <slug-or-id> -m \"Describe this deployment\"", remoteName, createSlug, remoteName)
	}
	bindingState := "this workspace was not rebound"
	if newlyBound {
		bindingState = "this workspace remains locally bound to the missing agent"
	}

	return fmt.Errorf("the agent bound to remote %q no longer exists in Airlock.\n\n  Agent: %s (%s)\n  Airlock: %s%s\n\nNo source was uploaded, and %s. If the deletion was intentional, run from %s:\n  go tool air remote unbind %s%s", remoteName, binding.Slug, binding.AgentID, baseURL, detail, bindingState, dir, remoteName, nextDeploy)
}

func visibleAgentBySlug(ctx context.Context, baseURL, token, slug string) (*airlockv1.AgentInfo, error) {
	if slug == "" {
		return nil, nil
	}
	var resp airlockv1.ListAgentsResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents", token, nil, &resp); err != nil {
		return nil, err
	}
	for _, agent := range resp.Agents {
		if agent.GetSlug() == slug {
			return agent, nil
		}
	}
	return nil, nil
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
	statusCode int
	gitRemote  string
	gitBranch  string
}

func (e *staleSourceError) Error() string { return "source state is stale" }

func deploySourceStateError(stale *staleSourceError, target agentRemoteBinding, baseURL, remoteName string) error {
	if stale.gitRemote != "" {
		branchArg := ""
		if stale.gitBranch != "" {
			branchArg = " --branch " + stale.gitBranch
		}
		return fmt.Errorf("the connected Git branch changed since this workspace last synced.\n\nClone the current branch into another directory:\n  git clone%s %s ../%s-latest\n\nMerge your changes there and push through Git", branchArg, stale.gitRemote, target.Slug)
	}
	if stale.statusCode == http.StatusPreconditionRequired {
		return fmt.Errorf("Airlock already has source for %s (%s), but this workspace has no synchronized source state.\n\nClone the current source into another directory:\n  airlock clone %s ../%s-airlock --remote %s --url %s\n\nMerge your changes into that directory, then deploy from there:\n  cd ../%s-airlock\n  go tool air deploy -m \"Describe this deployment\"\n\nUse --force only to replace Airlock's current source", target.Slug, target.AgentID, target.AgentID, target.Slug, remoteName, baseURL, target.Slug)
	}
	return fmt.Errorf("Airlock source changed since this workspace last synced.\n\nClone the current source into another directory:\n  airlock clone %s ../%s-airlock --remote %s --url %s\n\nMerge your changes into that directory, then deploy from there:\n  cd ../%s-airlock\n  go tool air deploy -m \"Describe this deployment\"\n\nUse --force only to replace Airlock's current source", target.AgentID, target.Slug, remoteName, baseURL, target.Slug)
}

func uploadSource(ctx context.Context, baseURL, token, agentID, dir, sourceState, commitMessage string, force bool) (string, error) {
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
	req.Header.Set("X-Airlock-Commit-Message", commitMessage)
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
			statusCode: resp.StatusCode,
			gitRemote:  strings.TrimSpace(resp.Header.Get("X-Airlock-Git-Remote")),
			gitBranch:  strings.TrimSpace(resp.Header.Get("X-Airlock-Git-Branch")),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload source: %w", newHTTPStatusError(resp.StatusCode, resp.Status, b))
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
