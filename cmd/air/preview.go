package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/wire"
)

func cmdPreview(args []string) error {
	if len(args) > 0 && args[0] == "configuration" {
		return cmdCandidateConfiguration(args[1:])
	}
	if len(args) > 0 && args[0] == "select" {
		return cmdPreviewSelect(args[1:])
	}
	for _, arg := range args {
		if arg == "--target" || strings.HasPrefix(arg, "--target=") {
			return errors.New("air preview selects the saved preview; use air deploy --target for another target")
		}
	}
	return deployCommand(append([]string{"--target", "preview"}, args...), true)
}

// Candidate configuration changes only the pending revision, allowing a blocked
// build to continue without mutating the running deployment's bindings.
func cmdCandidateConfiguration(args []string) error {
	var selectionArgs, binds, fixtures []string
	for i := 0; i < len(args); i++ {
		if args[i] != "--bind" && args[i] != "--fixture" {
			selectionArgs = append(selectionArgs, args[i])
			continue
		}
		flag := args[i]
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return fmt.Errorf("%s requires a selection", flag)
		}
		i++
		if flag == "--bind" {
			binds = append(binds, args[i])
		} else {
			fixtures = append(fixtures, args[i])
		}
	}
	f, pos, err := parseSelectionFlags(selectionArgs)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !deployUUIDRe.MatchString(pos[0]) || f.target != "" || f.wait {
		return errors.New("preview configuration requires a candidate revision UUID; use --bind/--fixture to set pending selections")
	}
	header, err := candidateConfigHeader(binds, fixtures)
	if err != nil {
		return err
	}
	f.target = "preview"
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	method := http.MethodGet
	var input *airlockv1.SetCandidateConfigurationRequest
	if header != "" {
		var config wire.CandidateConfig
		if err := json.Unmarshal([]byte(header), &config); err != nil {
			return err
		}
		input = &airlockv1.SetCandidateConfigurationRequest{}
		for _, entry := range config.Selections {
			input.Selections = append(input.Selections, &airlockv1.CandidateSelection{Type: entry.Type, Slug: entry.Slug, ResourceId: entry.ResourceID, FixtureName: entry.FixtureName})
		}
		method = http.MethodPut
	}
	var out airlockv1.CandidateConfigurationResponse
	path := s.path("/candidates/" + pos[0] + "/configuration")
	if input == nil {
		err = doProto(ctx, s.baseURL, method, path, s.token, nil, &out)
	} else {
		err = doProto(ctx, s.baseURL, method, path, s.token, input, &out)
	}
	if err != nil {
		return err
	}
	if out.SourceRevisionId != pos[0] {
		return errors.New("candidate configuration response selected a different revision")
	}
	return writeProtoJSON(&out)
}

func deployToTarget(f deployFlags) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		if f.create || os.Getenv("AIRLOCK_DEPLOY_APPROVED") != "1" {
			return errors.New("hosted developer deployment requires approval; rerun this exact command with request_permission=true")
		}
	}
	configHeader, err := candidateConfigHeader(f.binds, f.fixtures)
	if err != nil {
		return err
	}
	if f.create {
		binding, _, err := loadAgentBinding(f.dir)
		if err != nil {
			return err
		}
		name := f.remote
		if name == "" {
			name = binding.DefaultRemote
		}
		if name == "" {
			name = defaultRemoteName
		}
		bound, _ := binding.remote(name)
		if bound.AgentID != "" {
			return fmt.Errorf("remote %q already has an app; deploy without --create", name)
		}
		baseURL := normalizeBaseURL(f.url)
		if baseURL != "" && bound.AirlockURL != "" && baseURL != normalizeBaseURL(bound.AirlockURL) {
			return errors.New("--url conflicts with the selected remote")
		}
		if baseURL == "" {
			baseURL = bound.AirlockURL
		}
		if _, err := canonicalAuthOrigin(baseURL); err != nil {
			return err
		}
		token, err := accessTokenForURL(ctx, baseURL)
		if err != nil {
			return err
		}
		if _, err := ensureDeploySDKVersion(ctx, baseURL, token); err != nil {
			return err
		}
		app, err := createDraftAgent(ctx, baseURL, token, f)
		if err != nil {
			return explainCreateAgentError(ctx, baseURL, token, name, f.dir, f.slug, err)
		}
		binding.putRemote(name, app)
		if err := writeAgentBinding(f.dir, binding); err != nil {
			return err
		}
	}
	s, err := resolveDeployment(ctx, targetFlags{dir: f.dir, remote: f.remote, url: f.url, agent: f.agent, target: f.target, timeout: f.timeout}, f.ensurePreview)
	if err != nil {
		return err
	}
	if _, err := ensureDeploySDKVersion(ctx, s.baseURL, s.token); err != nil {
		return err
	}
	state := s.app.Targets[s.info.Id]
	etag, etagPinned := os.LookupEnv("AIRLOCK_DEPLOYMENT_SOURCE_ETAG")
	if etagPinned && os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		state.SourceETag = etag
	}
	// Hosted checkpoints exclude local CLI state. Observe this exact admitted
	// target when a restored workspace has no cached source version; existing
	// cached versions retain their normal conflict checks.
	if state.SourceETag == "" && !etagPinned && os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" && s.info.SourceRevisionId != "" {
		metadata, err := s.sourceMetadata(ctx)
		if err != nil {
			return err
		}
		state.SourceETag = metadata.ETag
	}
	if state.Name != "" && (state.Name != s.info.Name || state.OwnerUserID != s.info.OwnerUserId) {
		return errors.New("saved target source state has inconsistent ownership")
	}
	if state.PendingTask != "" {
		if err := s.reconcileTask(ctx, &state, false, 2*time.Second); err != nil {
			return err
		}
	}
	if f.force {
		metadata, err := s.sourceMetadata(ctx)
		if err != nil && !hasHTTPStatus(err, http.StatusNotFound) {
			return err
		}
		state.SourceETag = metadata.ETag
	}
	accepted, err := uploadSource(ctx, s.baseURL, s.token, s.path("/source"), f.dir, state.SourceETag, f.message, configHeader)
	if err != nil {
		var stale *staleSourceError
		if errors.As(err, &stale) {
			return fmt.Errorf("source conflict for remote %s target %s: pull this target before deploying, or explicitly use --force: %w", s.remoteName, f.target, err)
		}
		return err
	}
	s.expectedRevision = ""
	state.Name, state.OwnerUserID = s.info.Name, s.info.OwnerUserId
	if accepted.TaskID == "" {
		generation, err := strconv.ParseInt(accepted.Generation, 10, 64)
		if err != nil || generation < 0 || !deployUUIDRe.MatchString(accepted.Revision) {
			return errors.New("upload returned invalid source revision/generation")
		}
		state.SourceState = accepted.State
		state.SourceETag = (wire.SourceVersion{Revision: accepted.Revision, Generation: generation}).ETag()
		s.expectedRevision = accepted.Revision
	} else {
		if !deployUUIDRe.MatchString(accepted.TaskID) {
			return errors.New("upload returned invalid task identity")
		}
		state.PendingTask, state.PendingSourceState = accepted.TaskID, accepted.State
	}
	if err := s.saveSource(state); err != nil {
		return err
	}
	fmt.Printf("Remote: %s (%s)\nApp: %s\nTarget: %s\nRuntime: %s\nCandidate: %s\nTask: %s\n", s.remoteName, s.baseURL, s.app.AgentID, s.info.Name, s.info.Id, accepted.Revision, accepted.TaskID)
	// Builds preserve an explicitly stopped target. Launch waits for that
	// replacement before starting it, retaining the accepted task for recovery.
	if f.launch && s.info.Status == "stopped" && state.PendingTask != "" {
		if err := s.reconcileTask(ctx, &state, true, 2*time.Second); err != nil {
			return err
		}
	}
	// Upload admission checks source CAS and image availability before an
	// unchanged preview can resume its existing runtime. Active builds own startup.
	if f.launch && state.PendingTask == "" && s.info.ImageRef != "" && (s.info.Status == "stopped" || !s.info.GetRunning()) {
		if err := doProto(ctx, s.baseURL, http.MethodPost, s.path("/start"), s.token, nil, nil); err != nil {
			return err
		}
	}
	if !f.wait {
		return nil
	}
	if state.PendingTask != "" {
		if err := s.reconcileTask(ctx, &state, true, 2*time.Second); err != nil {
			return err
		}
	}
	if err := s.waitRunning(ctx, 2*time.Second); err != nil {
		return err
	}
	return s.print()
}

func candidateConfigHeader(binds, fixtures []string) (string, error) {
	if len(binds)+len(fixtures) == 0 {
		return "", nil
	}
	config := wire.CandidateConfig{Version: 1}
	for kind, values := range [][]string{binds, fixtures} {
		for _, value := range values {
			label, selection, ok := strings.Cut(value, "=")
			if !ok {
				return "", errors.New("candidate selection requires [type:]slot=resource-UUID or fixture-name")
			}
			typ, slot := "connection", label
			if left, right, found := strings.Cut(label, ":"); found {
				typ, slot = left, right
			}
			entry := wire.CandidateSelection{Type: typ, Slug: slot}
			if kind == 0 {
				entry.ResourceID = selection
			} else {
				entry.FixtureName = selection
			}
			config.Selections = append(config.Selections, entry)
		}
	}
	return config.HeaderValue()
}

func (s *selectedDeployment) saveSource(state targetSourceBinding) error {
	if s.app.Targets == nil {
		s.app.Targets = map[string]targetSourceBinding{}
	}
	s.app.Targets[s.info.Id] = state
	return s.persistRemote(false)
}

func (s selectedDeployment) sourceMetadata(ctx context.Context) (sourceMetadata, error) {
	response, err := doAuthenticatedHTTP(ctx, s.baseURL, s.token, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.baseURL+s.path("/source"), nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req, err
	})
	if err != nil {
		return sourceMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return sourceMetadata{}, newHTTPStatusError(response.StatusCode, response.Status, raw)
	}
	metadata := sourceMetadata{State: response.Header.Get(wire.SourceStateHeader), ETag: unquoteETag(response.Header.Get("ETag")), Revision: response.Header.Get(wire.SourceRevisionHeader), Generation: response.Header.Get(wire.SourceGenerationHeader)}
	version, err := wire.ParseSourceETag(metadata.ETag)
	if err != nil || metadata.State == "" || version.Revision != metadata.Revision || strconv.FormatInt(version.Generation, 10) != metadata.Generation {
		return sourceMetadata{}, errors.New("target source metadata is inconsistent")
	}
	return metadata, nil
}

func (s *selectedDeployment) reconcileTask(ctx context.Context, state *targetSourceBinding, wait bool, interval time.Duration) error {
	for {
		task, err := getDeploymentTask(ctx, s.baseURL, s.token, state.PendingTask)
		if err != nil {
			return err
		}
		switch task.Info.Status {
		case "queued", "running":
			if !wait {
				return fmt.Errorf("target deployment task %s is %s; use air status --target %s --wait", state.PendingTask, task.Info.Status, s.flags.target)
			}
		case "failed", "cancelled":
			id := state.PendingTask
			state.PendingTask, state.PendingSourceState = "", ""
			if err := s.saveSource(*state); err != nil {
				return err
			}
			return fmt.Errorf("deployment task %s %s: %s", id, task.Info.Status, task.Error)
		case "succeeded":
			if !deployUUIDRe.MatchString(task.BuildID) {
				return errors.New("completed deployment task omitted its build identity")
			}
			var build airlockv1.GetAgentBuildResponse
			if err := doProto(ctx, s.baseURL, http.MethodGet, s.path("/builds/"+task.BuildID), s.token, nil, &build); err != nil {
				return err
			}
			if build.Build == nil || build.Build.Id != task.BuildID || build.Build.AgentId != s.info.Id || build.Build.Status != "complete" {
				return errors.New("deployment task build does not match the selected runtime")
			}
			if err := s.refresh(ctx, http.MethodGet); err != nil {
				return err
			}
			if build.Build.SourceRef != "" && s.info.SourceRef != build.Build.SourceRef {
				return errors.New("selected target no longer runs the accepted task's built source")
			}
			metadata, err := s.sourceMetadata(ctx)
			if err != nil {
				return err
			}
			if metadata.Revision != s.info.SourceRevisionId {
				return errors.New("target source changed while reconciling deployment")
			}
			s.expectedRevision = metadata.Revision
			// Host housekeeping may produce a prepared source revision. Observe
			// the actual target canonical state, not a guessed uploaded digest.
			state.SourceState, state.SourceETag = metadata.State, metadata.ETag
			state.PendingTask, state.PendingSourceState = "", ""
			return s.saveSource(*state)
		default:
			return fmt.Errorf("unknown deployment task status %q", task.Info.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
