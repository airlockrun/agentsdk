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
	target, err := resolveIntegrationTarget(ctx, flags)
	if err != nil {
		return err
	}
	envVars, err := listEnvVars(ctx, target)
	if err != nil {
		return err
	}
	if command == "list" {
		return printEnvVars(envVars)
	}

	slug := positional[0]
	envVar, err := declaredEnvVar(envVars, slug)
	if err != nil {
		return err
	}
	if envVar.IsSecret {
		return fmt.Errorf("env var %q is secret; env %s only supports non-secret vars", slug, command)
	}
	path := envVarsPath(target) + "/" + url.PathEscape(slug)
	switch command {
	case "get":
		value := envVar.Value
		if !envVar.Configured {
			value = envVar.DefaultValue
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
		if err := doProto(ctx, target.baseURL, http.MethodPost, path, target.token, &airlockv1.SetEnvVarValueRequest{Value: value}, nil); err != nil {
			return err
		}
		fmt.Printf("Set %s\n", slug)
		return nil
	case "clear":
		if err := doProto(ctx, target.baseURL, http.MethodDelete, path, target.token, nil, nil); err != nil {
			return err
		}
		fmt.Printf("Cleared %s\n", slug)
		return nil
	}
	panic("unreachable")
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

func listEnvVars(ctx context.Context, target integrationTarget) ([]*airlockv1.EnvVarInfo, error) {
	var resp airlockv1.ListEnvVarsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, envVarsPath(target), target.token, nil, &resp); err != nil {
		return nil, err
	}
	return resp.EnvVars, nil
}

func envVarsPath(target integrationTarget) string {
	return "/api/v1/agents/" + url.PathEscape(target.agentID) + "/env-vars"
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
	fmt.Fprintln(w, "SLUG\tSECRET\tCONFIGURED\tDEFAULT\tPATTERN\tVALUE\tDESCRIPTION")
	for _, envVar := range envVars {
		defaultValue, value := envVar.DefaultValue, envVar.Value
		if envVar.IsSecret {
			defaultValue, value = "", ""
		}
		fmt.Fprintf(w, "%s\t%t\t%t\t%s\t%s\t%s\t%s\n",
			safeTableCell(envVar.Slug), envVar.IsSecret, envVar.Configured, safeTableCell(defaultValue),
			safeTableCell(envVar.Pattern), safeTableCell(value), safeTableCell(envVar.Description))
	}
	return w.Flush()
}

func safeTableCell(value string) string {
	quoted := strconv.QuoteToGraphic(value)
	return quoted[1 : len(quoted)-1]
}
