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
		fmt.Fprintln(os.Stderr, "Configuration readiness could not be inspected; deployment submission is unchanged.")
		fmt.Fprintf(os.Stderr, "Environment settings: %s\n", envSettingsURL(target))
		return
	}
	readiness := response.GetDeployment().GetConfigurationReadiness()
	if readiness == nil {
		fmt.Fprintln(os.Stderr, "Configuration readiness is unavailable from this host; inspect target settings after deployment exists.")
		fmt.Fprintf(os.Stderr, "Environment settings: %s\n", envSettingsURL(target))
		return
	}
	if !readiness.CandidateDeclarationsAvailable {
		fmt.Fprintln(os.Stderr, "Candidate declarations are not yet available; configuration report uses current target declarations.")
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
			fmt.Fprintf(os.Stderr, "Configuration notice — %s: %s\n", entry.label, strings.Join(slots, ", "))
		}
	}
	if !readiness.CandidateDeclarationsAvailable || len(readiness.UnboundResources)+len(readiness.UnsetEnvVars)+len(readiness.UnsetSecrets) > 0 {
		fmt.Fprintln(os.Stderr, "Configuration is available only after the target deployment exists. These notices do not block deployment; saving configuration does not restart a running target.")
		fmt.Fprintf(os.Stderr, "Environment settings: %s\nResource settings: %s/agents/%s?view=advanced&target=%s#connections\n", envSettingsURL(target), target.baseURL, url.PathEscape(target.app.AgentID), url.QueryEscape(target.info.Id))
	}
}
