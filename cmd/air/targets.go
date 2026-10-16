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
	json, wait                      bool
	timeout                         time.Duration
}

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
	s := selectedDeployment{flags: f}
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
	f := targetFlags{dir: ".", timeout: 10 * time.Minute}
	var positional []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
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
	if f.wait {
		state := s.app.Targets[s.info.Id]
		if state.PendingTask != "" {
			if err := s.reconcileTask(ctx, &state, true, 2*time.Second); err != nil {
				return err
			}
		}
		if err := s.waitRunning(ctx, 2*time.Second); err != nil {
			return err
		}
	}
	return s.print()
}

func (s *selectedDeployment) waitRunning(ctx context.Context, interval time.Duration) error {
	for {
		if err := s.refresh(ctx, http.MethodGet); err != nil {
			return err
		}
		if s.info.Status == "failed" || s.info.Status == "stopped" {
			return fmt.Errorf("deployment %s is %s", s.flags.target, s.info.Status)
		}
		if s.info.Status == "active" && s.info.Running != nil && s.info.GetRunning() && s.info.ImageRef != "" {
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
	if s.flags.json {
		raw, err := protoMarshal.Marshal(s.info)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Remote     string          `json:"remote"`
			Target     string          `json:"target"`
			Deployment json.RawMessage `json:"deployment"`
		}{Remote: s.remoteName, Target: s.info.Name, Deployment: raw})
	}
	running := "unknown"
	if s.info.Running != nil {
		running = fmt.Sprint(s.info.GetRunning())
	}
	fmt.Printf("Remote: %s (%s)\nApp: %s\nTarget: %s\nRuntime: %s\nOwner: %s\nStatus: %s\nRunning: %s\nRevision: %s\nURL: %s\n", s.remoteName, s.baseURL, s.app.AgentID, s.info.Name, s.info.Id, s.info.OwnerUserId, s.info.Status, running, s.info.SourceRevisionId, s.info.RouteBaseUrl)
	if s.info.OwnerWorkspaceId != "" {
		fmt.Printf("Owner hosted workspace: %s\n", s.info.OwnerWorkspaceId)
	}
	return nil
}

func cmdTargetLifecycle(action string, args []string) error {
	f, positional, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("lifecycle commands take no positional arguments; use --dir")
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	if err := doProto(ctx, s.baseURL, http.MethodPost, s.path("/"+action), s.token, nil, nil); err != nil {
		return err
	}
	if action == "start" && f.wait {
		if err := s.waitRunning(ctx, 2*time.Second); err != nil {
			return err
		}
	} else if err := s.refresh(ctx, http.MethodGet); err != nil {
		return err
	}
	return s.print()
}
