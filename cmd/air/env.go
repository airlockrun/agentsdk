package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"text/tabwriter"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

func cmdEnv(args []string) error {
	if len(args) == 0 {
		return errors.New("env requires: list, get <slug>, set <slug> <value>, or clear <slug>")
	}
	command := args[0]
	flags, positional, err := parseEnvArgs(args[1:])
	if err != nil {
		return err
	}
	switch command {
	case "list":
		if len(positional) != 0 {
			return errors.New("env list takes no arguments")
		}
	case "get", "clear":
		if len(positional) != 1 {
			return fmt.Errorf("env %s requires exactly one argument: <slug>", command)
		}
	case "set":
		if len(positional) != 2 {
			return errors.New("env set requires exactly two arguments: <slug> <value>")
		}
	default:
		return fmt.Errorf("unknown env subcommand %q", command)
	}
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return errors.New("env is unavailable with a codegen integration token; operator authentication is required")
	}

	ctx := context.Background()
	target, err := resolveDeployment(ctx, targetFlags{dir: ".", url: flags.url, remote: flags.remote, agent: flags.agent, target: flags.target}, false)
	if err != nil {
		return err
	}
	envVars, err := listEnvVars(ctx, target)
	if err != nil {
		return err
	}
	setSelectedContext(target)
	if command != "get" && invocationOutput == nil {
		target.printHeading()
	}
	if command == "list" {
		if invocationOutput != nil {
			for _, row := range envVars {
				if row.IsSecret {
					row.Value, row.DefaultValue = "", ""
				}
			}
			return writeProtoData(&airlockv1.ListEnvVarsResponse{EnvVars: envVars})
		}
		if err := printEnvVars(envVars); err != nil {
			return err
		}
		if link := envSettingsURL(target); link != "" {
			fmt.Printf("Settings: %s\n", link)
		}
		return nil
	}

	slug := positional[0]
	envVar, err := declaredEnvVar(envVars, slug)
	if err != nil {
		return err
	}
	if envVar.IsSecret && command == "get" {
		return fmt.Errorf("env var %q is secret; env %s only supports non-secret vars", slug, command)
	}
	path := envVarsPath(target) + "/" + url.PathEscape(slug)
	switch command {
	case "get":
		value := envVar.Value
		if !envVar.Configured {
			value = envVar.DefaultValue
		}
		if invocationOutput != nil {
			return writeJSONData(map[string]string{"slug": slug, "value": value})
		}
		fmt.Println(value)
		return nil
	case "set":
		value := positional[1]
		if envVar.Pattern != "" {
			pattern, err := regexp.Compile(envVar.Pattern)
			if err != nil {
				return fmt.Errorf("env var %q has an invalid declaration pattern: %w", slug, err)
			}
			if !pattern.MatchString(value) {
				return fmt.Errorf("value for env var %q does not match required pattern", slug)
			}
		}
		if invocationOutput != nil {
			invocationOutput.Operation = &cliOperation{Kind: "environment_configuration", Admission: "unknown", Action: "set", Slug: slug, RuntimeID: target.info.Id, Target: target.info.Name}
		}
		if err := doProto(ctx, target.baseURL, http.MethodPost, path, target.token, &airlockv1.SetEnvVarValueRequest{Value: value}, nil); err != nil {
			err = admissionFailure(err)
			if envVar.IsSecret {
				err = &redactedCLIError{cause: err, secret: value}
			}
			return fmt.Errorf("environment change for %q was not confirmed; inspect without replaying it: %s: %w", slug, shellCommand(selectedAction(target, "inspect", "")), err)
		}
		if invocationOutput != nil {
			invocationOutput.Operation.Admission = "accepted"
		}
		fmt.Printf("Saved %s. Run %s to check whether a restart is needed.\n", safeTableCell(slug), shellCommand(humanSelectedAction(target, "status")))
		setResult(map[string]any{"slug": slug, "configured": true, "runtimeConfiguration": "unobserved"}, "completed")
		return nil
	case "clear":
		if invocationOutput != nil {
			invocationOutput.Operation = &cliOperation{Kind: "environment_configuration", Admission: "unknown", Action: "clear", Slug: slug, RuntimeID: target.info.Id, Target: target.info.Name}
		}
		if err := doProto(ctx, target.baseURL, http.MethodDelete, path, target.token, nil, nil); err != nil {
			err = admissionFailure(err)
			return fmt.Errorf("environment change for %q was not confirmed; inspect without replaying it: %s: %w", slug, shellCommand(selectedAction(target, "inspect", "")), err)
		}
		if invocationOutput != nil {
			invocationOutput.Operation.Admission = "accepted"
		}
		fmt.Printf("Cleared %s. Run %s to check whether a restart is needed.\n", safeTableCell(slug), shellCommand(humanSelectedAction(target, "status")))
		setResult(map[string]any{"slug": slug, "configured": false, "runtimeConfiguration": "unobserved"}, "completed")
		return nil
	}
	panic("unreachable")
}

func envSettingsURL(target selectedDeployment) string {
	return workspaceSettingsURL(target, "environment")
}

func workspaceSettingsURL(target selectedDeployment, section string) string {
	if target.info.WorkspaceUrl != "" {
		link, err := url.Parse(target.info.WorkspaceUrl)
		if err == nil && link.Host != "" && (link.Scheme == "https" || link.Scheme == "http") && link.User == nil {
			query := link.Query()
			query.Set("settings", section)
			selector := target.info.Id
			if target.info.Kind == "production" {
				selector = "production"
			}
			query.Set("target", selector)
			link.RawQuery = query.Encode()
			return safeTableCell(link.String())
		}
	}
	return ""
}

func parseEnvArgs(args []string) (integrationTargetFlags, []string, error) {
	var flags integrationTargetFlags
	var positional []string
	options := true
	for i := 0; i < len(args); i++ {
		if options && args[i] == "--" {
			options = false
			continue
		}
		if options {
			handled, err := consumeIntegrationTargetFlag(args, &i, &flags)
			if err != nil {
				return integrationTargetFlags{}, nil, err
			}
			if handled {
				continue
			}
		}
		if options && len(args[i]) >= 2 && args[i][:2] == "--" {
			return integrationTargetFlags{}, nil, fmt.Errorf("unknown env flag %q", args[i])
		}
		positional = append(positional, args[i])
	}
	return flags, positional, nil
}

func listEnvVars(ctx context.Context, target selectedDeployment) ([]*airlockv1.EnvVarInfo, error) {
	var resp airlockv1.ListEnvVarsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, envVarsPath(target), target.token, nil, &resp); err != nil {
		return nil, err
	}
	return resp.EnvVars, nil
}

func envVarsPath(target selectedDeployment) string {
	return target.path("/env-vars")
}

func declaredEnvVar(envVars []*airlockv1.EnvVarInfo, slug string) (*airlockv1.EnvVarInfo, error) {
	for _, envVar := range envVars {
		if envVar.Slug == slug {
			return envVar, nil
		}
	}
	return nil, fmt.Errorf("env var %q is not declared by the agent", slug)
}

func printEnvVars(envVars []*airlockv1.EnvVarInfo) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tSECRET\tSTATE\tREQUIRED\tPRESENT\tDEFAULT\tPATTERN\tVALUE\tDESCRIPTION")
	for _, envVar := range envVars {
		defaultValue, value := envVar.DefaultValue, envVar.Value
		if envVar.IsSecret {
			defaultValue, value = "", ""
		}
		state := "unset"
		if envVar.Configured {
			state = "set"
		}
		fmt.Fprintf(w, "%s\t%t\t%s\t%t\t%t\t%s\t%s\t%s\t%s\n",
			safeTableCell(envVar.Slug), envVar.IsSecret, state, envVar.Required, envVar.Present, safeTableCell(defaultValue),
			safeTableCell(envVar.Pattern), safeTableCell(value), safeTableCell(envVar.Description))
	}
	return w.Flush()
}

func safeTableCell(value string) string {
	quoted := strconv.QuoteToGraphic(value)
	return quoted[1 : len(quoted)-1]
}
