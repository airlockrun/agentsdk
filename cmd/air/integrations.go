package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/goai/mcp"
	"google.golang.org/protobuf/proto"
)

type integrationTarget struct {
	baseURL string
	agentID string
	token   string
	codegen bool
}

func resolveIntegrationTarget(ctx context.Context) (integrationTarget, error) {
	baseURL := os.Getenv("AIRLOCK_API_URL")
	agentID := os.Getenv("AIRLOCK_AGENT_ID")
	token := os.Getenv("AIRLOCK_INTEGRATION_TOKEN")
	if baseURL != "" || agentID != "" || token != "" {
		if baseURL == "" || agentID == "" || token == "" {
			return integrationTarget{}, errors.New("AIRLOCK_API_URL, AIRLOCK_AGENT_ID, and AIRLOCK_INTEGRATION_TOKEN must be set together")
		}
		return integrationTarget{baseURL: normalizeBaseURL(baseURL), agentID: agentID, token: token, codegen: true}, nil
	}

	binding, ok, err := loadAgentBinding(".")
	if err != nil {
		return integrationTarget{}, err
	}
	if !ok {
		return integrationTarget{}, errors.New("workspace is not bound to Airlock; deploy or clone it first")
	}
	remote, ok := binding.remote("")
	if !ok || remote.AirlockURL == "" || remote.AgentID == "" {
		return integrationTarget{}, errors.New("workspace binding requires an Airlock URL and agent ID")
	}
	token, err = accessTokenForURL(ctx, remote.AirlockURL)
	if err != nil {
		return integrationTarget{}, err
	}
	return integrationTarget{baseURL: remote.AirlockURL, agentID: remote.AgentID, token: token}, nil
}

func (t integrationTarget) path(suffix string) string {
	if t.codegen {
		return "/api/codegen/integrations" + suffix
	}
	return "/api/v1/agents/" + url.PathEscape(t.agentID) + "/integrations" + suffix
}

func cmdIntegrations(args []string) error {
	if len(args) != 1 || args[0] != "list" {
		return errors.New("integrations requires: list")
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx)
	if err != nil {
		return err
	}
	var resp airlockv1.ListIntegrationsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, target.path("/"), target.token, nil, &resp); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tSLUG\tSTATUS\tDESCRIPTION")
	for _, item := range resp.Integrations {
		status := "not configured"
		if item.Configured {
			status = "configured"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Type, item.Slug, status, item.Description)
	}
	return w.Flush()
}

func cmdConnection(args []string) error {
	if len(args) < 2 || args[0] != "request" {
		return errors.New("connection requires: request <slug> --path <path> [--method <method>] [--data <body>] [--header <name:value>]")
	}
	slug := args[1]
	method, path, body := http.MethodGet, "", ""
	headers := map[string]string{}
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--method", "--path", "--data", "--header":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", args[i])
			}
			key, value := args[i], args[i+1]
			i++
			switch key {
			case "--method":
				method = value
			case "--path":
				path = value
			case "--data":
				body = value
			case "--header":
				name, headerValue, ok := strings.Cut(value, ":")
				if !ok || strings.TrimSpace(name) == "" {
					return errors.New("--header must be name:value")
				}
				headers[strings.TrimSpace(name)] = strings.TrimSpace(headerValue)
			}
		default:
			return fmt.Errorf("unknown connection flag %q", args[i])
		}
	}
	if path == "" {
		return errors.New("--path is required")
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx)
	if err != nil {
		return err
	}
	var resp airlockv1.InvokeConnectionResponse
	err = doProto(ctx, target.baseURL, http.MethodPost, target.path("/connections/"+url.PathEscape(slug)+"/request"), target.token, &airlockv1.InvokeConnectionRequest{
		Method: method, Path: path, Body: []byte(body), Headers: headers,
	}, &resp)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "HTTP %d\n", resp.StatusCode)
	if _, err = os.Stdout.Write(resp.Body); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func cmdExec(args []string) error {
	if len(args) < 4 || args[0] != "run" {
		return errors.New("exec requires: run <slug> [--timeout <duration>] -- <command> [args...]")
	}
	slug := args[1]
	timeout := time.Duration(0)
	separator := -1
	for i := 2; i < len(args); i++ {
		if args[i] == "--" {
			separator = i
			break
		}
		if args[i] != "--timeout" || i+1 >= len(args) {
			return fmt.Errorf("unknown exec flag %q", args[i])
		}
		parsed, err := time.ParseDuration(args[i+1])
		if err != nil {
			return fmt.Errorf("invalid timeout: %w", err)
		}
		if parsed <= 0 || parsed > 10*time.Minute {
			return errors.New("timeout must be greater than zero and at most 10m")
		}
		timeout = parsed
		i++
	}
	if separator < 0 || separator+1 >= len(args) {
		return errors.New("exec command must follow --")
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx)
	if err != nil {
		return err
	}
	var resp airlockv1.InvokeExecResponse
	err = doProto(ctx, target.baseURL, http.MethodPost, target.path("/exec/"+url.PathEscape(slug)+"/run"), target.token, &airlockv1.InvokeExecRequest{
		Command: args[separator+1], Args: args[separator+2:], TimeoutMs: timeout.Milliseconds(),
	}, &resp)
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(resp.Stdout); err != nil {
		return err
	}
	if _, err := os.Stderr.Write(resp.Stderr); err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("remote command exited %d", resp.ExitCode)
	}
	return nil
}

func cmdMCP(args []string) error {
	if len(args) == 0 {
		return errors.New("mcp requires: probe, tools, or call")
	}
	switch args[0] {
	case "probe":
		if len(args) != 2 {
			return errors.New("mcp probe requires exactly one URL")
		}
		return probeMCP(args[1])
	case "tools":
		if len(args) != 2 {
			return errors.New("mcp tools requires exactly one server slug")
		}
		return listMCPTools(args[1])
	case "call":
		if len(args) < 3 {
			return errors.New("mcp call requires: <server-slug> <tool> [--args <json>]")
		}
		arguments := "{}"
		if len(args) > 3 {
			if len(args) != 5 || args[3] != "--args" {
				return errors.New("mcp call accepts only --args <json>")
			}
			arguments = args[4]
		}
		if !json.Valid([]byte(arguments)) {
			return errors.New("MCP arguments must be valid JSON")
		}
		return callMCPTool(args[1], args[2], []byte(arguments))
	default:
		return fmt.Errorf("unknown mcp subcommand %q", args[0])
	}
}

func listMCPTools(slug string) error {
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx)
	if err != nil {
		return err
	}
	var resp airlockv1.ListIntegrationMCPToolsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, target.path("/mcp/"+url.PathEscape(slug)+"/tools"), target.token, nil, &resp); err != nil {
		return err
	}
	type listedTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	result := struct {
		Instructions string       `json:"instructions,omitempty"`
		Tools        []listedTool `json:"tools"`
	}{Instructions: resp.Instructions, Tools: make([]listedTool, len(resp.Tools))}
	for i, item := range resp.Tools {
		schema := json.RawMessage(item.InputSchemaJson)
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		result.Tools[i] = listedTool{Name: item.Name, Description: item.Description, InputSchema: schema}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func callMCPTool(slug, toolName string, arguments []byte) error {
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx)
	if err != nil {
		return err
	}
	var resp airlockv1.InvokeMCPToolResponse
	if err := doProto(ctx, target.baseURL, http.MethodPost, target.path("/mcp/"+url.PathEscape(slug)+"/call"), target.token, &airlockv1.InvokeMCPToolRequest{
		Tool: toolName, ArgumentsJson: arguments,
	}, &resp); err != nil {
		return err
	}
	if err := printProtoJSON(&resp); err != nil {
		return err
	}
	if resp.IsError {
		return errors.New("MCP tool returned an error")
	}
	return nil
}

func probeMCP(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.IsAbs() == false || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("MCP URL must be an absolute HTTP or HTTPS URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient()
	defer client.DisconnectAll()
	if err := client.Connect(ctx, mcp.ServerConfig{Name: "probe", Transport: "http", URL: rawURL}); err != nil {
		return err
	}
	type probeTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	result := struct {
		URL          string      `json:"url"`
		Instructions string      `json:"instructions,omitempty"`
		Tools        []probeTool `json:"tools"`
	}{URL: rawURL, Instructions: client.GetServerInstructions("probe")}
	for _, item := range client.GetTools().Ordered(nil) {
		result.Tools = append(result.Tools, probeTool{
			Name: strings.TrimPrefix(item.Name, "probe_"), Description: item.Description, InputSchema: item.InputSchema,
		})
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func printProtoJSON(message proto.Message) error {
	encoded, err := protoMarshal.Marshal(message)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, encoded, "", "  "); err != nil {
		return err
	}
	fmt.Println(indented.String())
	return nil
}
