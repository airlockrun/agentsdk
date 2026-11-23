package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

// reportDeploymentReadiness reads declaration metadata after submission. Missing
// configuration and inspection errors are visible diagnostics, never a new gate.
func reportDeploymentReadiness(ctx context.Context, target selectedDeployment, revision string) {
	var response airlockv1.GetDeploymentResponse
	path := target.path("") + "?candidate_revision_id=" + url.QueryEscape(revision)
	if err := doProto(ctx, target.baseURL, http.MethodGet, path, target.token, nil, &response); err != nil {
		if invocationOutput != nil {
			invocationOutput.Configuration = map[string]string{"availability": "inspection_failed"}
		}
		fmt.Fprintln(os.Stderr, "Configuration readiness could not be inspected; deployment submission is unchanged.")
		return
	}
	deployment := response.GetDeployment()
	if deployment == nil || deployment.Id != target.info.Id || deployment.AppId != target.app.AgentID || deployment.RuntimeAgentId != target.info.Id {
		if invocationOutput != nil {
			invocationOutput.Configuration = map[string]string{"availability": "inspection_failed"}
		}
		fmt.Fprintln(os.Stderr, "Configuration inspection returned inconsistent deployment identity; deployment submission is unchanged.")
		return
	}
	readiness := deployment.GetConfigurationReadiness()
	if readiness == nil {
		if invocationOutput != nil {
			invocationOutput.Configuration = map[string]string{"availability": "unknown"}
		}
		return
	}
	if invocationOutput != nil {
		source := "current"
		if readiness.CandidateDeclarationsAvailable {
			source = "candidate"
		}
		invocationOutput.Configuration = map[string]any{"requestedCandidateRevisionId": revision, "currentSourceRevisionId": deployment.SourceRevisionId, "declarationSource": source, "candidateDeclarationsAvailable": readiness.CandidateDeclarationsAvailable, "unboundResources": readiness.UnboundResources, "unsetEnvVars": readiness.UnsetEnvVars, "unsetSecrets": readiness.UnsetSecrets}
	}
	source := "Candidate"
	if !readiness.CandidateDeclarationsAvailable {
		source = "Current target (candidate declarations unavailable)"
	}
	for _, entry := range []struct {
		label string
		slots []string
	}{{"Unbound resources", readiness.UnboundResources}, {"Unset required environment values", readiness.UnsetEnvVars}, {"Unset required secrets", readiness.UnsetSecrets}} {
		if len(entry.slots) > 0 {
			slots := make([]string, len(entry.slots))
			for i, slot := range entry.slots {
				slots[i] = safeTableCell(slot)
			}
			fmt.Fprintf(os.Stderr, "%s — %s: %s\n", source, entry.label, strings.Join(slots, ", "))
		}
	}
	if len(readiness.UnboundResources)+len(readiness.UnsetEnvVars)+len(readiness.UnsetSecrets) > 0 {
		command := "env list"
		if len(readiness.UnsetEnvVars)+len(readiness.UnsetSecrets) == 0 {
			command = "resources bindings"
		}
		action := selectedAction(target, command, "Review current target declarations; pending candidate requirements may differ")
		action.Kind = "configure"
		if invocationOutput != nil {
			invocationOutput.Guidance = append(invocationOutput.Guidance, action)
		}
		fmt.Fprintf(os.Stderr, "Review current declarations: %s\n", shellCommand(action))
		if link := envSettingsURL(target); link != "" {
			fmt.Fprintf(os.Stderr, "Environment settings: %s\n", link)
		}
		if link := workspaceSettingsURL(target, "main"); link != "" {
			fmt.Fprintf(os.Stderr, "Resource settings: %s\n", link)
		}
	}
}
