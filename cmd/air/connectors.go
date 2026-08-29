package main

import (
	"context"
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
	if len(args) == 0 || (args[0] != "list" && args[0] != "inspect") {
		return errors.New("connectors requires: list [--url <url>] [--json] or inspect <id> [--url <url>] [--json]")
	}
	command := args[0]
	identifier, baseURL, jsonOutput := "", "", false
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
	if command == "list" && identifier != "" {
		return errors.New("connectors list takes no connector ID")
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
	if command == "list" {
		var response airlockv1.ListConnectorsResponse
		if err := doProto(ctx, resolvedURL, http.MethodGet, "/api/v1/connectors", token, nil, &response); err != nil {
			return err
		}
		if jsonOutput {
			return writeProtoJSON(&response)
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "ID\tDISPLAY NAME\tKIND\tREADINESS\tLAST SEEN")
		for _, item := range response.Connectors {
			lastSeen := "never"
			if item.LastSeenAt != nil {
				lastSeen = item.LastSeenAt.AsTime().Format("2006-01-02 15:04:05Z07:00")
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", item.Id, item.DisplayName, item.Kind, item.Readiness, lastSeen)
		}
		return writer.Flush()
	}
	var response airlockv1.GetConnectorResponse
	if err := doProto(ctx, resolvedURL, http.MethodGet, "/api/v1/connectors/"+url.PathEscape(identifier), token, nil, &response); err != nil {
		return err
	}
	if jsonOutput {
		return writeProtoJSON(&response)
	}
	item := response.Connector
	if item == nil {
		return errors.New("Airlock returned an empty connector")
	}
	fmt.Printf("ID: %s\nDisplay name: %s\nKind: %s\nContract: %s\nReadiness: %s\nArtifact: %s\nInterface hash: %s\nDescription: %s\nInterface:\n%s\n", item.Id, item.DisplayName, item.Kind, item.ContractId, item.Readiness, item.ArtifactVersion, item.InterfaceHash, item.Description, item.InterfaceJson)
	return nil
}

func connectorOperatorURL(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
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
	body, err := protoMarshal.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(body))
	return err
}
