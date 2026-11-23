package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/proto"
)

func cmdConnectors(args []string) error {
	if len(args) > 0 && (args[0] == "install" || args[0] == "update") {
		return cmdConnectorManagement(args)
	}
	if len(args) > 0 && args[0] == "list" {
		return errors.New("unknown connectors command \"list\"; use 'go tool air resources list' for global discovery or 'go tool air resources list preview' for app slots; connectors manages installations")
	}
	if len(args) == 0 || args[0] != "inspect" {
		return errors.New("connectors requires install, update or inspect; discover resources with 'go tool air resources list'")
	}
	command := args[0]
	identifier, baseURL, jsonOutput := "", "", invocationOutput != nil
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--url":
			if i+1 >= len(args) {
				return errors.New("--url requires a value")
			}
			baseURL = normalizeBaseURL(args[i+1])
			i++
		default:
			if strings.HasPrefix(args[i], "--") {
				return fmt.Errorf("unknown connectors flag %q", args[i])
			}
			if identifier != "" {
				return errors.New("connectors inspect accepts exactly one connector ID")
			}
			identifier = args[i]
		}
	}
	if command == "inspect" && identifier == "" {
		return errors.New("connectors inspect requires a connector ID")
	}
	ctx := context.Background()
	resolvedURL, err := connectorOperatorURL(baseURL)
	if err != nil {
		return err
	}
	token, err := accessTokenForURL(ctx, resolvedURL)
	if err != nil {
		return err
	}
	var response airlockv1.GetConnectorResponse
	if err := doProto(ctx, resolvedURL, http.MethodGet, "/api/v1/connectors/"+url.PathEscape(identifier), token, nil, &response); err != nil {
		return err
	}
	item := response.Connector
	if item == nil {
		return errors.New("Airlock returned an empty connector")
	}
	var latestJob *airlockv1.HostManagementJobInfo
	diagnosticsUnavailable := false
	if item.HostId != "" {
		var host airlockv1.GetHostResponse
		if err := doProto(ctx, resolvedURL, http.MethodGet, "/api/v1/hosts/"+url.PathEscape(item.HostId), token, nil, &host); err != nil {
			diagnosticsUnavailable = true
		} else {
			for _, job := range host.ManagementJobs {
				if job.ConnectorId != item.Id {
					continue
				}
				latestJob = job
				break
			}
		}
	}
	if jsonOutput {
		body, err := protoMarshal.Marshal(&response)
		if err != nil {
			return err
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal(body, &data); err != nil {
			return err
		}
		if latestJob != nil {
			job, err := protoMarshal.Marshal(latestJob)
			if err != nil {
				return err
			}
			data["latestManagementJob"] = job
		}
		if diagnosticsUnavailable {
			data["managementDiagnosticsUnavailable"] = json.RawMessage("true")
		}
		return writeJSONData(data)
	}
	fmt.Printf("Installation diagnostics\nID: %s\nHost: %s\nDisplay name: %s\nKind: %s\nContract: %s\nReadiness: %s\nReadiness detail: %s\nArtifact version: %s\nArtifact digest: %s\nInterface hash: %s\nDescription: %s\n", item.Id, item.HostId, item.DisplayName, item.Kind, item.ContractId, item.Readiness, item.ReadinessMessage, item.ArtifactVersion, item.ArtifactDigest, item.InterfaceHash, item.Description)
	if diagnosticsUnavailable {
		fmt.Println("Management diagnostics: unavailable; installation metadata is not a current management-job observation.")
	}
	if latestJob != nil {
		fmt.Printf("Latest management job: %s — %s (%s)\n", latestJob.Id, safeTableCell(latestJob.Kind), safeTableCell(latestJob.Status))
		if latestJob.ErrorMessage != "" {
			fmt.Printf("Management error: %s\n", safeTableCell(latestJob.ErrorMessage))
		}
		fmt.Printf("Observe: %s\n", shellCommand(cliAction{Executable: "go", Args: []string{"tool", "air", "hosts", "job", latestJob.Id, "--url", resolvedURL}}))
	}
	fmt.Printf("Interface:\n%s\n", item.InterfaceJson)
	return nil
}

func connectorOperatorURL(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		baseURL := normalizeBaseURL(os.Getenv("AIRLOCK_API_URL"))
		if _, err := canonicalAuthOrigin(baseURL); err != nil {
			return "", errors.New("developer management requires a valid AIRLOCK_API_URL")
		}
		return baseURL, nil
	}
	if binding, ok, err := loadAgentBinding("."); err != nil {
		return "", err
	} else if ok {
		if remote, found := binding.remote(binding.DefaultRemote); found && remote.AirlockURL != "" {
			return normalizeBaseURL(remote.AirlockURL), nil
		}
	}
	credentials, err := loadCredentials()
	if err != nil {
		return "", err
	}
	if len(credentials.Sessions) == 1 {
		for value := range credentials.Sessions {
			return value, nil
		}
	}
	if len(credentials.Sessions) == 0 {
		return "", errors.New("connectors command needs an Airlock URL: pass --url after login")
	}
	values := make([]string, 0, len(credentials.Sessions))
	for value := range credentials.Sessions {
		values = append(values, value)
	}
	sort.Strings(values)
	return "", fmt.Errorf("multiple Airlock logins found; pass --url with one of: %s", strings.Join(values, ", "))
}

func writeProtoJSON(value proto.Message) error {
	if invocationOutput != nil {
		if response, ok := value.(*airlockv1.GetHostManagementJobResponse); ok {
			return writeHostObservation(response)
		}
		return writeProtoData(value)
	}
	if humanOutput {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		switch response := value.(type) {
		case *airlockv1.InvokeMCPToolResponse:
			if response.IsError {
				fmt.Println("Resource tool returned an error.")
			}
			for _, content := range response.Content {
				if content.Text != "" {
					for _, line := range strings.Split(strings.TrimSuffix(content.Text, "\n"), "\n") {
						fmt.Println(safeTableCell(line))
					}
				} else {
					fmt.Printf("Content: %s (%s) — use --json for encoded content\n", safeTableCell(content.Type), safeTableCell(content.MimeType))
				}
			}
			if len(response.StructuredContentJson) > 0 {
				data, err := decodeJSONValue(response.StructuredContentJson)
				if err != nil {
					return fmt.Errorf("decode MCP structured content: %w", err)
				}
				renderHumanData(os.Stdout, data, "")
			}
			return nil
		case *airlockv1.DeploymentTestRunResponse:
			row := response.Test
			if row == nil {
				return errors.New("test response omitted its run")
			}
			fmt.Printf("Suite %s — %s\nRun: %s\n", safeTableCell(row.Suite), safeTableCell(row.Status), row.Id)
			if row.Error != "" {
				fmt.Printf("Error: %s\n", safeTableCell(row.Error))
			}
			if row.Status == "succeeded" || row.Status == "failed" {
				fmt.Printf("Exit: %d\n", row.ExitCode)
			}
			return nil
		case *airlockv1.ListToolsResponse:
			fmt.Fprintln(w, "TOOL\tACCESS\tDESCRIPTION")
			for _, row := range response.Tools {
				fmt.Fprintf(w, "%s\t%s\t%s\n", safeTableCell(row.Name), safeTableCell(row.Access), safeTableCell(row.Description))
			}
		case *airlockv1.ListOwnedResourcesResponse:
			fmt.Fprintln(w, "ADDRESS\tTYPE\tNAME\tAUTHORIZED\tCAPABILITIES\tREADINESS\tVERSION")
			for _, row := range response.Resources {
				address := row.Slug
				if address == "" {
					address = row.Id
				}
				name := row.DisplayName
				if name == "" {
					name = row.Name
				}
				status := row.GetConnectorStatus()
				fmt.Fprintf(w, "/%s\t%s\t%s\t%t\t%s\t%s\t%s\n", safeTableCell(address), safeTableCell(row.Type), safeTableCell(name), row.Authorized, safeTableCell(strings.Join(row.Capabilities, ",")), safeTableCell(status.GetReadiness()), safeTableCell(status.GetArtifactVersion()))
			}
		case *airlockv1.ListTargetBindingsResponse:
			fmt.Fprintln(w, "SLOT\tTYPE\tSELECTION\tSOURCE")
			for _, row := range response.Bindings {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", safeTableCell(row.Slug), safeTableCell(row.Type), safeTableCell(row.Selection), row.SourceKind.String())
			}
		default:
			return writeProtoData(value)
		}
		return w.Flush()
	}
	body, err := protoMarshal.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(body))
	return err
}
