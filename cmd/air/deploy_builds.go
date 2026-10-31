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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type deployBuildFlags struct {
	dir, remote, url, agent, build, target string
	limit                                  int
	json, watch, logs                      bool
}

func parseDeployBuildFlags(command string, args []string) (deployBuildFlags, error) {
	f := deployBuildFlags{dir: ".", limit: 10}
	dirSet, options := false, true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if options && a == "--" {
			options = false
			continue
		}
		if !options || !strings.HasPrefix(a, "-") {
			if dirSet {
				return f, fmt.Errorf("deploy %s takes at most one directory", command)
			}
			f.dir, dirSet = a, true
			continue
		}
		switch a {
		case "--json":
			f.json = true
			continue
		case "--watch", "--logs":
			if command != "status" {
				return f, fmt.Errorf("%s requires deploy status", a)
			}
			if a == "--watch" {
				f.watch = true
			} else {
				f.logs = true
			}
			continue
		case "--remote", "--url", "--agent", "--target":
		case "--limit":
			if command != "list" {
				return f, errors.New("--limit requires deploy list")
			}
		case "--build":
			if command != "status" {
				return f, errors.New("--build requires deploy status")
			}
		default:
			return f, fmt.Errorf("unknown deploy %s flag %q", command, a)
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
			return f, fmt.Errorf("flag %s needs a value", a)
		}
		i++
		switch a {
		case "--remote":
			f.remote = args[i]
		case "--url":
			f.url = args[i]
		case "--agent":
			f.agent = args[i]
		case "--target":
			f.target = args[i]
		case "--build":
			if !deployUUIDRe.MatchString(args[i]) {
				return f, errors.New("--build must be a UUID")
			}
			f.build = strings.ToLower(args[i])
		case "--limit":
			limit, err := strconv.Atoi(args[i])
			if err != nil || limit < 1 || limit > 50 {
				return f, errors.New("--limit must be between 1 and 50")
			}
			f.limit = limit
		}
	}
	if f.remote != "" && !validRemoteName(f.remote) {
		return f, fmt.Errorf("invalid remote %q", f.remote)
	}
	if f.watch && f.json {
		return f, errors.New("deploy status cannot combine --watch and --json")
	}
	return f, nil
}

func cmdDeployBuilds(command string, args []string) error {
	f, err := parseDeployBuildFlags(command, args)
	if err != nil {
		return err
	}
	if err := requireTarget(f.target); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runDeployBuilds(ctx, command, f, 2*time.Second)
}

func runDeployBuilds(ctx context.Context, command string, f deployBuildFlags, pollInterval time.Duration) error {
	if pollInterval <= 0 {
		return errors.New("build poll interval must be positive")
	}
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return errors.New("deploy is unavailable with a codegen integration token")
	}
	s, err := resolveDeployment(ctx, targetFlags{dir: f.dir, remote: f.remote, url: f.url, agent: f.agent, target: f.target}, false)
	if err != nil {
		return err
	}
	baseURL, token := s.baseURL, s.token
	fmt.Fprintf(os.Stderr, "Target: remote=%s app=%s target=%s runtime=%s\n", s.remoteName, s.app.AgentID, s.info.Name, s.info.Id)
	if !f.json {
		s.printAppURL()
	}
	target := agentRemoteBinding{AgentID: s.info.Id, Slug: s.info.Name}
	path := s.path("/builds")
	state := s.app.Targets[s.info.Id]
	if command == "status" && f.build == "" && state.PendingTask != "" {
		for {
			task, taskErr := getDeploymentTask(ctx, baseURL, token, state.PendingTask)
			if taskErr != nil {
				return fmt.Errorf("get deployment task: %w", taskErr)
			}
			if f.json && task.Info.Status != "succeeded" {
				if err := writeBuildObservation(task.Info, s.info); err != nil {
					return err
				}
			} else if !f.json {
				fmt.Printf("Task: %s\nTask kind: %s\nTask status: %s\n", strings.TrimPrefix(task.Info.Id, "task:"), safeTableCell(task.Info.Kind), safeTableCell(task.Info.Status))
				if task.Error != "" {
					fmt.Printf("Error: %s\n", safeTableCell(task.Error))
				}
			}
			switch task.Info.Status {
			case "queued", "running":
				if !f.watch {
					return nil
				}
				timer := time.NewTimer(pollInterval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			case "failed", "cancelled":
				taskID := state.PendingTask
				state.PendingTask, state.PendingSourceState = "", ""
				if err := s.saveSource(state); err != nil {
					return err
				}
				if task.Error == "" {
					task.Error = task.Info.Status
				}
				return fmt.Errorf("deployment task %s %s: %s", taskID, task.Info.Status, task.Error)
			case "succeeded":
				if err := s.reconcileTask(ctx, &state, false, pollInterval); err != nil {
					return err
				}
				if !deployUUIDRe.MatchString(task.BuildID) {
					return errors.New("completed deployment task omitted its build ID")
				}
				f.build = task.BuildID
			default:
				return fmt.Errorf("deployment task has unknown status %q", task.Info.Status)
			}
			break
		}
	}
	if command == "list" || f.build == "" {
		var resp airlockv1.ListAgentBuildsResponse
		if err := doProto(ctx, baseURL, http.MethodGet, path, token, nil, &resp); err != nil {
			return fmt.Errorf("list builds: %w", err)
		}
		if len(resp.Builds) == 0 {
			return fmt.Errorf("no builds found for %s (%s)", target.Slug, target.AgentID)
		}
		if command == "list" {
			if len(resp.Builds) > f.limit {
				resp.Builds = resp.Builds[:f.limit]
			}
			if f.json {
				return writeBuildObservation(&resp, s.info)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "BUILD ID\tTYPE\tSTATUS\tSTARTED\tMESSAGE")
			for _, b := range resp.Builds {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", safeTableCell(b.GetId()), safeTableCell(b.GetType()), safeTableCell(b.GetStatus()), buildTime(b.GetStartedAt()), safeTableCell(b.GetInstructions()))
			}
			return w.Flush()
		}
		f.build = resp.Builds[0].GetId()
		if !deployUUIDRe.MatchString(f.build) {
			return errors.New("latest build response did not include a valid build UUID")
		}
	}
	var previous *airlockv1.AgentBuildInfo
	for {
		var resp airlockv1.GetAgentBuildResponse
		if err := doProto(ctx, baseURL, http.MethodGet, path+"/"+url.PathEscape(f.build), token, nil, &resp); err != nil {
			return fmt.Errorf("get build: %w", err)
		}
		b := resp.Build
		if b.GetId() != f.build || b.GetAgentId() != target.AgentID {
			return errors.New("build response does not match the requested build and agent")
		}
		if f.json {
			if err := writeBuildObservation(&resp, s.info); err != nil {
				return err
			}
		} else {
			// Logs are cumulative snapshots, independent of lifecycle changes.
			snapshot := proto.Clone(b).(*airlockv1.AgentBuildInfo)
			snapshot.DockerLog, snapshot.SolLog, snapshot.LogSeq = "", "", 0
			var oldSnapshot *airlockv1.AgentBuildInfo
			if previous != nil {
				oldSnapshot = proto.Clone(previous).(*airlockv1.AgentBuildInfo)
				oldSnapshot.DockerLog, oldSnapshot.SolLog, oldSnapshot.LogSeq = "", "", 0
			}
			if !proto.Equal(snapshot, oldSnapshot) {
				printBuildStatus(b, target, baseURL)
			}
			if f.logs {
				printBuildLog("Docker", previous.GetDockerLog(), b.DockerLog)
				printBuildLog("Sol", previous.GetSolLog(), b.SolLog)
			}
		}
		switch b.Status {
		case "failed":
			return fmt.Errorf("build %s failed", b.Id)
		case "complete":
			return nil
		case "building":
		default:
			return fmt.Errorf("build %s has unknown status %q", b.Id, b.Status)
		}
		if !f.watch {
			return nil
		}
		previous = b
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Build observations retain protobuf field names alongside the selected target's
// app origin and explicit workspace link. Neither asserts that a candidate is deployed.
func writeBuildObservation(response proto.Message, deployment *airlockv1.DeploymentInfo) error {
	raw, err := protoMarshal.Marshal(response)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	fields["workspaceUrl"], err = json.Marshal(deployment.WorkspaceUrl)
	if err != nil {
		return err
	}
	fields["routeBaseUrl"], err = json.Marshal(deployment.RouteBaseUrl)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(fields)
}

func buildTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return "-"
	}
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

func printBuildStatus(b *airlockv1.AgentBuildInfo, target agentRemoteBinding, baseURL string) {
	fmt.Printf("Build: %s\nTarget: %s (%s) at %s\nType: %s\nBuild status: %s\nDeployment phase: %s\nStarted: %s\nFinished: %s\nSource ref: %s\nMessage: %s\n",
		safeTableCell(b.Id), safeTableCell(target.Slug), target.AgentID, safeTableCell(baseURL), safeTableCell(b.Type), safeTableCell(b.Status),
		strings.TrimPrefix(b.DeploymentPhase.String(), "AGENT_BUILD_DEPLOYMENT_PHASE_"), buildTime(b.StartedAt), buildTime(b.FinishedAt), safeTableCell(b.SourceRef), safeTableCell(b.Instructions))
	if b.DeploymentPausedAt != nil {
		fmt.Printf("Deployment paused: %s\n", buildTime(b.DeploymentPausedAt))
	}
	if b.DeploymentDrainDeadline != nil {
		fmt.Printf("Drain deadline: %s\n", buildTime(b.DeploymentDrainDeadline))
	}
	if b.ErrorMessage != "" {
		fmt.Printf("Error: %s\n", safeTableCell(b.ErrorMessage))
	}
	if b.FailureKind != "" {
		fmt.Printf("Failure kind: %s\n", safeTableCell(b.FailureKind))
	}
	if b.ExitStatus != "" || b.ExitMessage != "" {
		fmt.Printf("Sol exit: %s %s\n", safeTableCell(b.ExitStatus), safeTableCell(b.ExitMessage))
	}
	for _, blocker := range b.JobBlockers {
		fmt.Printf("Job blocker: %s v%d queued=%d running=%d input=%s output=%s\n", safeTableCell(blocker.GetHandlerName()), blocker.GetHandlerVersion(), blocker.GetQueuedCount(), blocker.GetRunningCount(), safeTableCell(blocker.GetInputSchemaHash()), safeTableCell(blocker.GetOutputSchemaHash()))
	}
}

func printBuildLog(name, previous, current string) {
	if previous == current {
		return
	}
	delta := current
	if strings.HasPrefix(current, previous) {
		delta = current[len(previous):]
	} else {
		fmt.Printf("[%s log snapshot reset]\n", name)
	}
	if delta != "" {
		fmt.Printf("[%s log]\n%s", name, delta)
		if !strings.HasSuffix(delta, "\n") {
			fmt.Println()
		}
	}
}
