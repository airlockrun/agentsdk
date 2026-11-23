package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/proto"
)

type targetFlags struct {
	dir, remote, url, agent, target string
	revision                        string
	json, wait                      bool
	timeout                         time.Duration
}

type readinessScope uint8

const (
	readinessDeployment readinessScope = iota
	readinessRuntime
)

type selectedDeployment struct {
	flags            targetFlags
	remoteName       string
	baseURL          string
	token            string
	app              agentRemoteBinding
	info             *airlockv1.DeploymentInfo
	binding          agentBinding
	expectedRevision string
	selector         string
	workspaceID      string
}

func requireTarget(target string) error {
	if target == "" {
		return nil
	}
	if !validRemoteName(target) {
		return fmt.Errorf("invalid deployment target %q", target)
	}
	return nil
}

func defaultTarget(target string) string {
	if target != "" {
		return target
	}
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		return "preview"
	}
	return "production"
}

func (s selectedDeployment) path(suffix string) string {
	selector := s.selector
	if selector == "" {
		selector = s.flags.target
	}
	return "/api/v1/agents/" + url.PathEscape(s.app.AgentID) + "/deployments/" + url.PathEscape(selector) + suffix
}

func resolveDeployment(ctx context.Context, f targetFlags, ensure bool) (selectedDeployment, error) {
	f.target = defaultTarget(f.target)
	s := selectedDeployment{flags: f, expectedRevision: f.revision}
	if err := requireTarget(f.target); err != nil {
		return s, err
	}
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return s, errors.New("deployment targets require user authentication, not a codegen integration token")
	}
	if f.target == "preview" && ensure && os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") == "" {
		release, err := lockPreviewSelection(ctx, f.dir)
		if err != nil {
			return s, err
		}
		defer release()
	}
	// Hosted work must carry logical identity explicitly. AIRLOCK_AGENT_ID is
	// a concrete runtime identity and cannot select a logical application.
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		appID := strings.TrimSpace(os.Getenv("AIRLOCK_APP_ID"))
		runtimeID := strings.TrimSpace(os.Getenv("AIRLOCK_AGENT_ID"))
		target := strings.TrimSpace(os.Getenv("AIRLOCK_DEPLOYMENT_TARGET"))
		workspaceID := strings.TrimSpace(os.Getenv("AIRLOCK_DEVELOPER_SESSION_ID"))
		remoteName := "hosted"
		baseURL := normalizeBaseURL(os.Getenv("AIRLOCK_API_URL"))
		if !deployUUIDRe.MatchString(appID) || !deployUUIDRe.MatchString(runtimeID) || !deployUUIDRe.MatchString(workspaceID) || target == "" || baseURL == "" {
			return s, errors.New("hosted deployment requires exact app/runtime/target/developer-session coordinates")
		}
		if target == "preview" || (deployUUIDRe.MatchString(f.agent) && f.agent != appID) || (f.url != "" && normalizeBaseURL(f.url) != baseURL) {
			return s, errors.New("CLI target conflicts with the admitted hosted app/preview context")
		}
		binding, _, err := loadAgentBinding(f.dir)
		if err != nil {
			return s, err
		}
		if f.remote != "" {
			remoteName = f.remote
		}
		s.binding, s.remoteName, s.baseURL = binding, remoteName, baseURL
		bound, _ := binding.remote(s.remoteName)
		if remoteName != "hosted" && (normalizeBaseURL(bound.AirlockURL) != baseURL || bound.AgentID != appID) {
			return s, errors.New("selected remote conflicts with the admitted hosted origin/app")
		}
		if bound.AirlockURL != "" && normalizeBaseURL(bound.AirlockURL) != baseURL {
			return s, errors.New("workspace remote conflicts with the admitted hosted origin/workspace")
		}
		if bound.AgentID != "" && bound.AgentID != appID {
			return s, errors.New("workspace logical app conflicts with hosted context")
		}
		s.app = agentRemoteBinding{AirlockURL: baseURL, AgentID: appID, Targets: bound.Targets, Previews: bound.Previews}
		s.workspaceID = workspaceID
		s.selector = f.target
		injected := f.target == "preview" || f.target == target || f.target == runtimeID
		if injected {
			s.selector = target
		}
		s.token, err = accessTokenForURL(ctx, baseURL)
		if err != nil {
			return s, err
		}
		if f.agent != "" && f.agent != appID {
			app, err := resolveAgentTarget(ctx, baseURL, s.token, f.agent, remoteName, agentRemoteBinding{})
			if err != nil {
				return s, err
			}
			if app.AgentID != appID {
				return s, errors.New("selected app conflicts with the admitted hosted app")
			}
		}
		if err := s.refresh(ctx, http.MethodGet); err != nil {
			return s, err
		}
		if injected && s.info.Id != runtimeID {
			return s, errors.New("resolved target does not match the admitted hosted runtime")
		}
		if injected && s.info.OwnerWorkspaceId != workspaceID {
			return s, errors.New("hosted target ownership does not match injected workspace")
		}
		return s, nil
	}
	binding, _, err := loadAgentBinding(f.dir)
	if err != nil {
		return s, err
	}
	s.binding = binding
	s.remoteName = f.remote
	if s.remoteName == "" {
		s.remoteName = binding.DefaultRemote
	}
	if s.remoteName == "" {
		s.remoteName = defaultRemoteName
	}
	bound, _ := binding.remote(s.remoteName)
	s.baseURL = normalizeBaseURL(f.url)
	if s.baseURL != "" && bound.AirlockURL != "" && s.baseURL != normalizeBaseURL(bound.AirlockURL) {
		return s, fmt.Errorf("remote %q is bound to %s, not %s", s.remoteName, bound.AirlockURL, s.baseURL)
	}
	if s.baseURL == "" {
		s.baseURL = bound.AirlockURL
	}
	if s.baseURL == "" {
		return s, errors.New("an explicit Airlock remote or --url is required")
	}
	if _, err := canonicalAuthOrigin(s.baseURL); err != nil {
		return s, err
	}
	s.token, err = accessTokenForURL(ctx, s.baseURL)
	if err != nil {
		return s, err
	}
	appBinding := bound
	// An explicit logical app override selects another app on this remote;
	// it does not silently change the remote's default logical app binding.
	if f.agent != "" {
		appBinding.AgentID = ""
	}
	s.app, err = resolveAgentTarget(ctx, s.baseURL, s.token, f.agent, s.remoteName, appBinding)
	if err != nil {
		return s, explainDeployTargetError(ctx, s.baseURL, s.token, s.remoteName, f.dir, f.agent, bound, err)
	}
	s.app.Targets = bound.Targets
	s.app.Previews = bound.Previews
	method := http.MethodGet
	if ensure {
		if f.target != "preview" {
			return s, errors.New("only preview can be ensured automatically")
		}
	}
	if f.target == "preview" {
		if ref, ok := s.app.Previews[s.app.AgentID]; ok {
			s.selector = ref.DeploymentID
		} else if ensure {
			method = http.MethodPost
		} else {
			return s, errors.New("this remote/app has no selected preview; run air preview first")
		}
	}
	if err := s.refresh(ctx, method); err != nil {
		return s, err
	}
	if ensure {
		if err := s.savePreviewReference(); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s *selectedDeployment) refresh(ctx context.Context, method string) error {
	var out airlockv1.GetDeploymentResponse
	var input proto.Message
	if method == http.MethodPost {
		input = &airlockv1.CreatePreviewRequest{}
	}
	err := doProto(ctx, s.baseURL, method, s.path(""), s.token, input, &out)
	if err != nil {
		return fmt.Errorf("resolve %s deployment at remote %s: %w", s.flags.target, s.remoteName, err)
	}
	d := out.Deployment
	if d == nil || d.AppId != s.app.AgentID || !deployUUIDRe.MatchString(d.Id) || d.Id != d.RuntimeAgentId {
		return errors.New("deployment response has inconsistent app/runtime identity")
	}
	if s.flags.target == "preview" {
		if d.Kind != "personal_preview" || !validRemoteName(d.Name) || d.Name == "preview" || d.Name == "production" {
			return errors.New("preview resolution did not return a concrete preview target")
		}
	} else if d.Name != s.flags.target && d.Id != s.flags.target {
		return errors.New("deployment response returned a different target")
	}
	if s.info != nil && (s.info.Id != d.Id || s.info.OwnerWorkspaceId != d.OwnerWorkspaceId || s.info.OwnerUserId != d.OwnerUserId) {
		return errors.New("deployment identity changed during operation")
	}
	if s.flags.target == "preview" && method == http.MethodGet && s.workspaceID == "" {
		ref := s.app.Previews[s.app.AgentID]
		if ref.DeploymentID != d.Id || ref.Name != d.Name {
			return errors.New("selected preview response changed target identity")
		}
	}
	if s.expectedRevision != "" && d.SourceRevisionId != s.expectedRevision {
		return errors.New("selected target source changed while observing the accepted deployment")
	}
	s.info = d
	s.selector = d.Name
	return nil
}

func parseTargetFlags(args []string) (targetFlags, []string, error) {
	f, positional, err := parseSelectionFlags(args)
	if err != nil {
		return f, positional, err
	}
	return f, positional, requireTarget(f.target)
}

func parseSelectionFlags(args []string) (targetFlags, []string, error) {
	f := targetFlags{dir: ".", timeout: 10 * time.Minute, json: invocationOutput != nil}
	var positional []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			positional = append(positional, arg)
			continue
		}
		if seen[arg] {
			return f, nil, fmt.Errorf("duplicate flag %s", arg)
		}
		seen[arg] = true
		if arg == "--json" {
			f.json = true
			continue
		}
		if arg == "--wait" {
			f.wait = true
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
			return f, nil, fmt.Errorf("%s requires a value", arg)
		}
		i++
		switch arg {
		case "--dir":
			f.dir = args[i]
		case "--remote":
			f.remote = args[i]
		case "--url":
			f.url = args[i]
		case "--agent":
			f.agent = args[i]
		case "--target":
			f.target = args[i]
		case "--revision":
			if !deployUUIDRe.MatchString(args[i]) {
				return f, nil, errors.New("--revision requires a canonical serving source UUID")
			}
			f.revision = args[i]
		case "--timeout":
			value, err := time.ParseDuration(args[i])
			if err != nil || value <= 0 {
				return f, nil, errors.New("--timeout requires a positive duration")
			}
			f.timeout = value
		default:
			return f, nil, fmt.Errorf("unknown flag %s", arg)
		}
	}
	if f.remote != "" && !validRemoteName(f.remote) {
		return f, nil, errors.New("invalid remote name")
	}
	return f, positional, nil
}

func cmdTargetStatus(args []string) error {
	f, positional, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return errors.New("status takes at most one directory")
	}
	if len(positional) == 1 {
		f.dir = positional[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	setSelectedContext(s)
	if invocationOutput != nil {
		if err := s.print(); err != nil {
			return err
		}
	}
	if f.wait {
		scope := readinessDeployment
		if f.revision != "" {
			scope = readinessRuntime
		}
		state := s.app.Targets[s.info.Id]
		if state.PendingTask != "" && f.revision == "" {
			if invocationOutput != nil {
				invocationOutput.Operation = &cliOperation{Kind: "source_upload", Admission: "accepted", TaskID: state.PendingTask, RuntimeID: s.info.Id, Target: s.info.Name}
			}
			if err := s.reconcileTask(ctx, &state, true, 2*time.Second); err != nil {
				observationErr := s.observationFailure(ctx, err)
				s.statusGuidance(ctx, false, observationErr)
				return observationErr
			}
			scope = readinessRuntime
		}
		if err := s.waitRunning(ctx, 2*time.Second, scope); err != nil {
			observationErr := s.observationFailure(ctx, err)
			s.statusGuidance(ctx, false, observationErr)
			return observationErr
		}
	}
	if err := s.print(); err != nil {
		return err
	}
	s.statusGuidance(ctx, f.wait, nil)
	return nil
}

// Status guidance follows the final snapshot/error, not intermediate print calls.
// It is observation-only and never changes cached source/admission state.
func (s selectedDeployment) statusGuidance(ctx context.Context, completedWait bool, observationErr error) {
	if completedWait {
		return
	}
	var failed *deploymentTaskFailure
	if s.info.Status == "failed" || s.info.BuildTaskStatus == "failed" || s.info.BuildTaskStatus == "cancelled" || errors.As(observationErr, &failed) {
		action := selectedAction(s, "deploy status", "Inspect the failed update's build diagnostics without resubmitting", "--logs")
		if invocationOutput != nil {
			invocationOutput.Guidance = []cliAction{action}
		}
		if !s.flags.json {
			fmt.Printf("Build details: %s\n", shellCommand(humanSelectedAction(s, "deploy status", "--logs")))
		}
		return
	}
	if s.info.Status == "stopped" || s.info.Status == "suspended" {
		return
	}
	interrupted := errors.Is(observationErr, context.DeadlineExceeded) || errors.Is(observationErr, context.Canceled)
	if observationErr != nil && !interrupted {
		return
	}
	pending := s.info.BuildTaskStatus == "queued" || s.info.BuildTaskStatus == "running"
	confirmedTask := pending && deployUUIDRe.MatchString(s.info.BuildTaskId)
	if deployUUIDRe.MatchString(s.info.PendingCandidateRevisionId) && s.info.PendingCandidateRevisionId != s.info.SourceRevisionId {
		pending = true
	}
	// A cached handle alone does not establish that its task is still in progress.
	state := s.app.Targets[s.info.Id]
	saved := state.PendingTask
	if !confirmedTask && deployUUIDRe.MatchString(saved) && state.Name == s.info.Name && state.OwnerUserID == s.info.OwnerUserId && (s.info.BuildTaskId == "" || s.info.BuildTaskId == saved) && s.info.BuildTaskStatus != "succeeded" && ctx.Err() == nil {
		task, err := getDeploymentTask(ctx, s.baseURL, s.token, saved)
		if err != nil {
			if invocationOutput != nil {
				invocationOutput.Warnings = append(invocationOutput.Warnings, cliWarning{Code: "status_task_inspection_unavailable", Message: "The saved deployment task could not be checked; inspect before waiting"})
			}
			fmt.Fprintln(os.Stderr, "The saved deployment task could not be checked; inspect before waiting.")
			return
		}
		if (task.Info.Kind == "build" || task.Info.Kind == "upgrade") && (task.Info.Status == "queued" || task.Info.Status == "running") {
			pending = true
		}
	}
	if !pending {
		return
	}
	extra := []string{"--wait"}
	if s.flags.timeout != 10*time.Minute {
		extra = append(extra, "--timeout", s.flags.timeout.String())
	}
	if s.flags.revision != "" {
		extra = append(extra, "--revision", s.flags.revision)
	}
	message, label := "Observe this update's readiness without resubmitting", "Wait for this update"
	if interrupted {
		message, label = "Resume bounded observation without resubmitting; remote work may continue", "Resume observation (do not resubmit)"
	}
	if invocationOutput != nil {
		invocationOutput.Guidance = []cliAction{selectedAction(s, "status", message, extra...), selectedAction(s, "inspect", "Inspect the same deployment")}
	}
	if !s.flags.json {
		fmt.Printf("%s: %s\n", label, shellCommand(humanSelectedAction(s, "status", extra...)))
	}
}

func deploymentObservationError(s selectedDeployment, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("observation stopped; deployment may continue. Inspect without resubmitting: %s: %w", shellCommand(selectedAction(s, "inspect", "")), err)
	}
	return err
}

func (s *selectedDeployment) observationFailure(ctx context.Context, err error) error {
	var failed *deploymentTaskFailure
	if !errors.As(err, &failed) {
		return deploymentObservationError(*s, err)
	}
	if refreshErr := s.refresh(ctx, http.MethodGet); refreshErr != nil {
		if invocationOutput != nil {
			invocationOutput.Warnings = append(invocationOutput.Warnings, cliWarning{Code: "runtime_inspection_unavailable", Message: "Current runtime state could not be inspected; data is the last observed snapshot"})
		}
		fmt.Fprintln(os.Stderr, "The update failed; current runtime state could not be inspected. Do not resubmit without inspection.")
		return err
	}
	if s.flags.json {
		if printErr := s.print(); printErr != nil {
			return errors.Join(err, printErr)
		}
		return err
	}
	s.printHeading()
	label := "Observed update"
	if failed.id != "" && failed.id == s.info.BuildTaskId {
		label = "Latest update"
	}
	fmt.Printf("%s %s.\n", label, safeTableCell(failed.status))
	if s.info.Running == nil {
		fmt.Println("Current runtime state is unknown.")
	} else if s.info.GetRunning() {
		fmt.Println("The current version is still running.")
	} else {
		fmt.Println("The current runtime is not running.")
	}
	s.printAppURL()
	return err
}

// Runtime observation converges on an active running image under any pinned
// source revision. Deployment observation also pins the current task/candidate.
func (s *selectedDeployment) waitRunning(ctx context.Context, interval time.Duration, scope readinessScope) error {
	if scope != readinessDeployment && scope != readinessRuntime {
		panic("invalid CLI readiness scope")
	}
	taskID := s.info.GetBuildTaskId()
	candidateID := s.info.GetPendingCandidateRevisionId()
	for {
		if err := s.refresh(ctx, http.MethodGet); err != nil {
			return err
		}
		if invocationOutput != nil && s.flags.json {
			if err := s.print(); err != nil {
				return err
			}
		}
		if scope == readinessDeployment && taskID != "" && s.info.BuildTaskId != "" && taskID != s.info.BuildTaskId {
			return errors.New("deployment task changed while observing readiness; inspect the selected target without resubmitting")
		}
		if scope == readinessDeployment && candidateID != "" && s.info.PendingCandidateRevisionId != "" && candidateID != s.info.PendingCandidateRevisionId {
			return errors.New("deployment candidate changed while observing readiness; inspect the selected target without resubmitting")
		}
		if taskID == "" {
			taskID = s.info.BuildTaskId
		}
		if candidateID == "" {
			candidateID = s.info.PendingCandidateRevisionId
		}
		if scope == readinessDeployment && (s.info.BuildTaskStatus == "failed" || s.info.BuildTaskStatus == "cancelled") {
			return &deploymentTaskFailure{id: s.info.BuildTaskId, status: s.info.BuildTaskStatus}
		}
		if s.info.Status == "failed" || s.info.Status == "stopped" {
			return fmt.Errorf("deployment %s is %s", s.flags.target, s.info.Status)
		}
		updateSettled := s.info.BuildTaskStatus != "queued" && s.info.BuildTaskStatus != "running" && (s.info.PendingCandidateRevisionId == "" || s.info.PendingCandidateRevisionId == s.info.SourceRevisionId)
		if s.info.Status == "active" && s.info.Running != nil && s.info.GetRunning() && s.info.ImageRef != "" && (scope == readinessRuntime || updateSettled) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (s selectedDeployment) print() error {
	setSelectedContext(s)
	info := s.info
	if info.EnvConfigurationState == "" {
		info = proto.Clone(info).(*airlockv1.DeploymentInfo)
		info.EnvConfigurationState = "unknown"
	}
	if s.flags.json {
		raw, err := protoMarshal.Marshal(info)
		if err != nil {
			return err
		}
		return writeJSONData(struct {
			Remote      string          `json:"remote"`
			Target      string          `json:"target"`
			Deployment  json.RawMessage `json:"deployment"`
			PendingTask string          `json:"pendingTaskId,omitempty"`
		}{Remote: s.remoteName, Target: s.info.Name, Deployment: raw, PendingTask: s.app.Targets[s.info.Id].PendingTask})
	}
	s.printHeading()
	if (s.info.Status == "failed" || s.info.BuildTaskStatus == "failed" || s.info.BuildTaskStatus == "cancelled") && s.info.GetRunning() {
		if s.info.BuildTaskStatus == "cancelled" {
			fmt.Println("Latest update was cancelled. The current version is still running.")
		} else if s.info.BuildTaskStatus == "failed" {
			fmt.Println("Latest update failed. The current version is still running.")
		} else {
			fmt.Println("Deployment failed. A runtime is still running; inspect its revision before treating it as the attempted update.")
		}
	} else if s.app.Targets[s.info.Id].PendingTask != "" || s.info.BuildTaskStatus == "queued" || s.info.BuildTaskStatus == "running" || (s.info.PendingCandidateRevisionId != "" && s.info.PendingCandidateRevisionId != s.info.SourceRevisionId) {
		label := "Update submitted."
		if s.info.BuildTaskStatus == "running" {
			label = "Building update."
		}
		if s.info.BuildTaskStatus == "queued" {
			label = "Update queued."
		}
		fmt.Println(label)
		if s.info.GetRunning() {
			fmt.Println("The current version is still running.")
		}
	} else if s.info.Status == "active" && s.info.GetRunning() && s.info.ImageRef != "" {
		fmt.Println("Ready")
	} else {
		fmt.Printf("Deployment: %s\n", safeTableCell(s.info.Status))
		if s.info.GetRunning() {
			fmt.Println("Runtime is running; check the latest update before treating it as ready.")
		}
	}
	s.printAppURL()
	if info.RestartRequired {
		fmt.Println("Restart needed to apply saved environment changes.")
	} else if info.EnvConfigurationState == "rebuild_required" {
		fmt.Println("Rebuild needed to verify environment configuration.")
	} else if info.EnvConfigurationState == "next_start" {
		fmt.Println("Saved environment changes apply on the next start.")
	}
	return nil
}

func (s selectedDeployment) printHeading() {
	fmt.Println(s.heading())
}

func (s selectedDeployment) heading() string {
	app := s.app.displayName
	if app == "" {
		app = s.app.Slug
	}
	if app == "" {
		app = "Application"
	}
	target := s.info.Name
	if s.info.Kind == "personal_preview" {
		target = "Preview"
	}
	return fmt.Sprintf("%s · %s · %s", safeTableCell(app), safeTableCell(target), safeTableCell(s.remoteName))
}

func cmdTargetInspect(args []string) error {
	f, positional, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if f.wait {
		return errors.New("inspect does not wait; use status --wait")
	}
	if len(positional) > 1 {
		return errors.New("inspect takes at most one directory")
	}
	if len(positional) == 1 {
		f.dir = positional[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	setSelectedContext(s)
	if f.json {
		return writeProtoData(s.info)
	}
	if err := s.print(); err != nil {
		return err
	}
	fmt.Printf("App ID: %s\nTarget selector: %s\nRuntime: %s\nOwner: %s\nWorkspace: %s\nSource revision: %s\nSource ref: %s\nImage: %s\nRuntime generation: %d\nConfiguration revision: %d\nSDK: %s\nTask: %s (%s)\nPending candidate: %s\nBuild phase: %s\n", s.app.AgentID, s.info.Name, s.info.Id, s.info.OwnerUserId, s.info.OwnerWorkspaceId, s.info.SourceRevisionId, s.info.SourceRef, s.info.ImageRef, s.info.RuntimeGeneration, s.info.ConfigRevision, s.info.SdkVersion, s.info.BuildTaskId, s.info.BuildTaskStatus, s.info.PendingCandidateRevisionId, strings.TrimPrefix(s.info.BuildPhase.String(), "AGENT_BUILD_DEPLOYMENT_PHASE_"))
	configuration, restart := s.info.EnvConfigurationState, fmt.Sprint(s.info.RestartRequired)
	if configuration == "" || configuration == "unknown" {
		configuration, restart = "unknown", "unknown"
	}
	fmt.Printf("Environment configuration: %s\nRestart required: %s\n", safeTableCell(configuration), restart)
	return nil
}

func (s selectedDeployment) printAppURL() {
	if s.info.RouteBaseUrl != "" {
		fmt.Printf("App URL: %s\n", s.info.RouteBaseUrl)
	} else {
		fmt.Println("App URL: unavailable (server returned no route URL for this target)")
	}
}

func cmdTargetLifecycle(action string, args []string) error {
	f, positional, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("lifecycle commands take no positional arguments; use --dir")
	}
	if f.wait && action != "start" && action != "restart" {
		return fmt.Errorf("--wait is not supported by %s", action)
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	setSelectedContext(s)
	if invocationOutput != nil {
		invocationOutput.Operation = &cliOperation{Kind: "lifecycle", Action: action, RuntimeID: s.info.Id, Target: s.info.Name, Admission: "unknown"}
	}
	if err := doProto(ctx, s.baseURL, http.MethodPost, s.path("/"+action), s.token, nil, nil); err != nil {
		return admissionFailure(err)
	}
	if invocationOutput != nil {
		invocationOutput.Operation.Admission = "accepted"
	}
	if (action == "start" || action == "restart") && f.wait {
		if err := s.waitRunning(ctx, 2*time.Second, readinessRuntime); err != nil {
			return fmt.Errorf("runtime observation stopped; remote work may continue. Inspect without resubmitting: %s: %w", shellCommand(selectedAction(s, "inspect", "")), err)
		}
	} else if err := s.refresh(ctx, http.MethodGet); err != nil {
		return err
	}
	if !f.wait && !f.json {
		fmt.Printf("%s requested. Current runtime snapshot:\n", action)
	}
	if f.wait && !f.json {
		fmt.Printf("%s completed for the current runtime. Deployment update state is shown separately.\n", action)
	}
	if err := s.print(); err != nil {
		return err
	}
	if invocationOutput != nil && !f.wait {
		invocationOutput.Status = "accepted"
	}
	return nil
}
