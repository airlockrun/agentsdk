package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

type developerSessionResponse struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
}

type developerResourceRequest struct {
	ID            string `json:"id"`
	SessionID     string `json:"sessionId"`
	Type          string `json:"type"`
	RequestedSlug string `json:"requestedSlug"`
	ResourceID    string `json:"resourceId"`
	Status        string `json:"status"`
	SetupURL      string `json:"setupUrl"`
	ErrorMessage  string `json:"errorMessage"`
}

type developerResourceResponse struct {
	Request developerResourceRequest `json:"request"`
}

type developerResourceListResponse struct {
	Requests []developerResourceRequest `json:"requests"`
}

type developerResourceCallResponse struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

type resourceFlags struct {
	target                    integrationTargetFlags
	session, typ, name        string
	baseURL, authMode, header string
	resourceID                string
	method, path, body        string
	requestHeaders            []string
	json                      bool
}

func cmdResources(args []string) error {
	if len(args) == 0 {
		return errors.New("resources requires: request, list, status, bind, or call")
	}
	command := args[0]
	f, positional, err := parseResourceFlags(args[1:])
	if err != nil {
		return err
	}
	switch command {
	case "request":
		if len(positional) != 1 {
			return errors.New("resources request requires one need slug")
		}
	case "list":
		if len(positional) != 0 {
			return errors.New("resources list takes no arguments")
		}
	case "status", "bind", "call":
		if len(positional) != 1 {
			return fmt.Errorf("resources %s requires one request ID", command)
		}
	default:
		return fmt.Errorf("unknown resources subcommand %q", command)
	}
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return errors.New("resources requires developer user authentication, not a codegen integration token")
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx, f.target)
	if err != nil {
		return err
	}
	sessionID, err := ensureDeveloperSession(ctx, target, f)
	if err != nil {
		return err
	}
	path := "/api/v1/developer-sessions/" + url.PathEscape(sessionID) + "/resources"
	switch command {
	case "request":
		return requestDeveloperResource(ctx, target, sessionID, path, positional[0], f)
	case "list", "status":
		var response developerResourceListResponse
		if err := doJSON(ctx, target.baseURL, http.MethodGet, path, target.token, nil, &response); err != nil {
			return err
		}
		if command == "status" {
			id := positional[0]
			for _, request := range response.Requests {
				if request.ID == id {
					response.Requests = []developerResourceRequest{request}
					return printDeveloperResources(response.Requests, f.json)
				}
			}
			return fmt.Errorf("developer resource request %s not found", id)
		}
		return printDeveloperResources(response.Requests, f.json)
	case "bind":
		requestID := positional[0]
		if !deployUUIDRe.MatchString(requestID) {
			return errors.New("resource request ID must be a UUID")
		}
		var response developerResourceResponse
		if err := doJSON(ctx, target.baseURL, http.MethodPost, path+"/"+url.PathEscape(requestID)+"/bind", target.token, nil, &response); err != nil {
			return err
		}
		return printDeveloperResources([]developerResourceRequest{response.Request}, f.json)
	case "call":
		requestID := positional[0]
		if !deployUUIDRe.MatchString(requestID) {
			return errors.New("resource request ID must be a UUID")
		}
		if f.method == "" || f.path == "" {
			return errors.New("resources call requires --method and --path")
		}
		headers := make(map[string]string, len(f.requestHeaders))
		for _, raw := range f.requestHeaders {
			name, value, ok := strings.Cut(raw, ":")
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("invalid --request-header %q; use 'Name: value'", raw)
			}
			headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
		var response developerResourceCallResponse
		body := map[string]any{"method": strings.ToUpper(f.method), "path": f.path, "headers": headers, "body": f.body}
		if err := doJSON(ctx, target.baseURL, http.MethodPost, path+"/"+url.PathEscape(requestID)+"/call", target.token, body, &response); err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(response.Body)
		if err != nil {
			return fmt.Errorf("decode resource response body: %w", err)
		}
		if f.json {
			encoded, err := json.MarshalIndent(map[string]any{"statusCode": response.StatusCode, "headers": response.Headers, "body": string(decoded)}, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(encoded))
			return nil
		}
		fmt.Printf("HTTP %d\n", response.StatusCode)
		for name, value := range response.Headers {
			fmt.Printf("%s: %s\n", name, value)
		}
		if len(decoded) > 0 {
			fmt.Printf("\n%s\n", decoded)
		}
		return nil
	}
	panic("unreachable")
}

func parseResourceFlags(args []string) (resourceFlags, []string, error) {
	f := resourceFlags{typ: "connection", authMode: "none"}
	var positional []string
	for i := 0; i < len(args); i++ {
		if handled, err := consumeIntegrationTargetFlag(args, &i, &f.target); handled || err != nil {
			if err != nil {
				return resourceFlags{}, nil, err
			}
			continue
		}
		if args[i] == "--json" {
			f.json = true
			continue
		}
		if !strings.HasPrefix(args[i], "--") {
			positional = append(positional, args[i])
			continue
		}
		if i+1 >= len(args) {
			return resourceFlags{}, nil, fmt.Errorf("flag %s needs a value", args[i])
		}
		key, value := args[i], args[i+1]
		i++
		switch key {
		case "--session":
			f.session = value
		case "--type":
			f.typ = value
		case "--name":
			f.name = value
		case "--base-url":
			f.baseURL = value
		case "--auth-mode":
			f.authMode = value
		case "--header":
			f.header = value
		case "--resource":
			f.resourceID = value
		case "--method":
			f.method = value
		case "--path":
			f.path = value
		case "--body":
			f.body = value
		case "--request-header":
			f.requestHeaders = append(f.requestHeaders, value)
		default:
			return resourceFlags{}, nil, fmt.Errorf("unknown resources flag %q", key)
		}
	}
	return f, positional, nil
}

func ensureDeveloperSession(ctx context.Context, target integrationTarget, f resourceFlags) (string, error) {
	hosted := strings.TrimSpace(os.Getenv("AIRLOCK_DEVELOPMENT_SESSION"))
	if hosted != "" {
		if f.session != "" && f.session != hosted {
			return "", errors.New("--session does not match AIRLOCK_DEVELOPMENT_SESSION")
		}
		return hosted, nil
	}
	if f.session != "" {
		if !deployUUIDRe.MatchString(f.session) {
			return "", errors.New("--session must be a UUID")
		}
		return f.session, nil
	}
	binding, ok, err := loadAgentBinding(".")
	if err != nil || !ok {
		if err == nil {
			err = errors.New("workspace is not bound to Airlock")
		}
		return "", err
	}
	remoteName := f.target.remote
	if remoteName == "" {
		remoteName = binding.DefaultRemote
	}
	remote, _ := binding.remote(remoteName)
	if remote.DeveloperSession != "" {
		return remote.DeveloperSession, nil
	}
	var response developerSessionResponse
	err = doJSON(ctx, target.baseURL, http.MethodGet, "/api/v1/developer-sessions?app_id="+url.QueryEscape(target.agentID), target.token, nil, &response)
	if hasHTTPStatus(err, http.StatusNotFound) {
		err = doJSON(ctx, target.baseURL, http.MethodPost, "/api/v1/developer-sessions", target.token, map[string]any{"appId": target.agentID}, &response)
	}
	if err != nil {
		return "", err
	}
	if !deployUUIDRe.MatchString(response.Session.ID) {
		return "", errors.New("create developer session response omitted its session ID")
	}
	remote.DeveloperSession = response.Session.ID
	binding.putRemote(remoteName, remote)
	if err := writeAgentBinding(".", binding); err != nil {
		return "", err
	}
	return response.Session.ID, nil
}

func requestDeveloperResource(ctx context.Context, target integrationTarget, sessionID, path, slug string, f resourceFlags) error {
	if f.typ != "connection" && f.typ != "mcp_server" {
		return errors.New("--type must be connection or mcp_server")
	}
	resourceID := f.resourceID
	if resourceID == "" {
		if f.typ != "connection" {
			return errors.New("new MCP resources require --resource with an existing discovered MCP server")
		}
		if strings.TrimSpace(f.name) == "" || strings.TrimSpace(f.baseURL) == "" {
			return errors.New("new connections require --name and --base-url")
		}
		request := &airlockv1.CreateConnectionResourceRequest{DisplayName: f.name, BaseUrl: f.baseURL}
		switch f.authMode {
		case "none":
			request.AuthMode = "none"
		case "bearer":
			request.AuthMode, request.AuthInjectionType = "token", "bearer"
		case "api_key":
			if strings.TrimSpace(f.header) == "" {
				return errors.New("--auth-mode api_key requires --header, for example X-API-Key")
			}
			request.AuthMode, request.AuthInjectionType, request.AuthInjectionName = "token", "api_key_header", f.header
		default:
			return errors.New("--auth-mode must be none, bearer, or api_key")
		}
		var created airlockv1.CreateConnectionResourceResponse
		if err := doProto(ctx, target.baseURL, http.MethodPost, "/api/v1/resources/connections/setup", target.token, request, &created); err != nil {
			return err
		}
		resourceID = created.Id
	}
	if !deployUUIDRe.MatchString(resourceID) {
		return errors.New("resource ID must be a UUID")
	}
	requestID, err := newUUID()
	if err != nil {
		return err
	}
	var response developerResourceResponse
	body := map[string]any{"requestId": requestID, "type": f.typ, "requestedSlug": slug, "resourceId": resourceID}
	if runID := strings.TrimSpace(os.Getenv("AIRLOCK_DEVELOPER_RUN_ID")); runID != "" {
		if !deployUUIDRe.MatchString(runID) {
			return errors.New("AIRLOCK_DEVELOPER_RUN_ID must be a UUID")
		}
		body["developerRunId"] = runID
	}
	if err := doJSON(ctx, target.baseURL, http.MethodPost, path, target.token, body, &response); err != nil {
		return fmt.Errorf("link resource %s to developer session: %w", resourceID, err)
	}
	return printDeveloperResources([]developerResourceRequest{response.Request}, f.json)
}

func printDeveloperResources(requests []developerResourceRequest, asJSON bool) error {
	if asJSON {
		encoded, err := json.MarshalIndent(developerResourceListResponse{Requests: requests}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REQUEST ID\tTYPE\tNEED\tSTATUS\tRESOURCE ID\tSETUP URL\tERROR")
	for _, request := range requests {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", request.ID, request.Type, request.RequestedSlug, request.Status, request.ResourceID, request.SetupURL, safeTableCell(request.ErrorMessage))
	}
	return w.Flush()
}
