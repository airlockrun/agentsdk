package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/proto"
)

// Resource inventory/direct use is user+remote scoped. Binding commands also
// require app+target; neither operation establishes a developer session.
func cmdResources(args []string) error {
	if len(args) == 0 {
		return errors.New("resources requires list, inspect, request, mcp-tools, mcp-call, bindings, bind, fixture or unbind")
	}
	command := args[0]
	resource, fixture, revision, data, method, path, tool := "", "", "", "", "GET", "", ""
	var targetArgs []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--resource", "--fixture", "--revision", "--data", "--method", "--path", "--tool":
			key := args[i]
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", key)
			}
			i++
			switch key {
			case "--resource":
				resource = args[i]
			case "--fixture":
				fixture = args[i]
			case "--revision":
				revision = args[i]
			case "--data":
				data = args[i]
			case "--method":
				method = args[i]
			case "--path":
				path = args[i]
			case "--tool":
				tool = args[i]
			}
		default:
			targetArgs = append(targetArgs, args[i])
		}
	}
	isBinding := command == "bindings" || command == "bind" || command == "fixture" || command == "unbind"
	if !isBinding {
		for _, arg := range targetArgs {
			if arg == "--target" || arg == "--agent" {
				return errors.New("direct resource commands are user+remote scoped; use resources bindings|bind|fixture|unbind for app targets")
			}
		}
	}
	f, positional, err := parseSelectionFlags(targetArgs)
	if err != nil {
		return err
	}
	if f.wait {
		return errors.New("resource commands do not support --wait")
	}
	if isBinding {
		if err := requireTarget(f.target); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	if isBinding {
		s, err := resolveDeployment(ctx, f, false)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Target: remote=%s app=%s target=%s runtime=%s\n", s.remoteName, s.app.AgentID, s.info.Name, s.info.Id)
		if command == "bindings" {
			if len(positional) != 0 {
				return errors.New("resources bindings takes no positional arguments")
			}
			var response airlockv1.ListTargetBindingsResponse
			if err := doProto(ctx, s.baseURL, http.MethodGet, s.path("/bindings"), s.token, nil, &response); err != nil {
				return err
			}
			return writeProtoJSON(&response)
		}
		if len(positional) != 2 {
			return errors.New("resources bind|fixture|unbind requires <type> <slot>")
		}
		var input proto.Message
		verb := http.MethodPut
		switch command {
		case "bind":
			if !deployUUIDRe.MatchString(resource) || fixture != "" {
				return errors.New("bind requires --resource <UUID>, without --fixture")
			}
			input = &airlockv1.SetTargetBindingRequest{ResourceId: resource}
		case "fixture":
			if fixture == "" || !deployUUIDRe.MatchString(revision) || resource != "" {
				return errors.New("fixture requires --fixture <name> --revision <candidate UUID>, without --resource")
			}
			input = &airlockv1.SetTargetBindingRequest{FixtureName: fixture, SourceRevisionId: revision}
		case "unbind":
			if resource != "" || fixture != "" {
				return errors.New("unbind takes no resource/fixture selection")
			}
			verb = http.MethodDelete
		}
		var response airlockv1.ListTargetBindingsResponse
		if err := doProto(ctx, s.baseURL, verb, s.path("/bindings/"+url.PathEscape(positional[0])+"/"+url.PathEscape(positional[1])), s.token, input, &response); err != nil {
			return err
		}
		if response.RuntimeAgentId != s.info.Id {
			return errors.New("binding response selected a different runtime")
		}
		return writeProtoJSON(&response)
	}
	baseURL, token, err := resolveUserRemote(ctx, f)
	if err != nil {
		return err
	}
	switch command {
	case "list":
		if len(positional) != 0 {
			return errors.New("resources list takes no positional arguments")
		}
		var response airlockv1.ListOwnedResourcesResponse
		if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/resources", token, nil, &response); err != nil {
			return err
		}
		return writeProtoJSON(&response)
	case "inspect":
		if len(positional) != 2 || !deployUUIDRe.MatchString(positional[1]) {
			return errors.New("resources inspect requires <type> <resource UUID>")
		}
		var response airlockv1.OwnedResourceInfo
		if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/resources/"+url.PathEscape(positional[0])+"/"+positional[1], token, nil, &response); err != nil {
			return err
		}
		return writeProtoJSON(&response)
	case "request":
		if len(positional) != 1 || !deployUUIDRe.MatchString(positional[0]) || path == "" {
			return errors.New("resources request requires resource UUID --path /path [--method METHOD] [--data body]")
		}
		var response airlockv1.InvokeConnectionResponse
		if err := doProto(ctx, baseURL, http.MethodPost, "/api/v1/resources/connections/"+positional[0]+"/request", token, &airlockv1.InvokeConnectionRequest{Method: method, Path: path, Body: []byte(data)}, &response); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "HTTP %d\n", response.StatusCode)
		if _, err := os.Stdout.Write(response.Body); err != nil {
			return err
		}
		if response.StatusCode >= 400 {
			return fmt.Errorf("resource returned HTTP %d", response.StatusCode)
		}
		return nil
	case "mcp-tools", "mcp-call":
		if len(positional) != 1 || !deployUUIDRe.MatchString(positional[0]) {
			return errors.New("MCP resource command requires resource UUID")
		}
		root := "/api/v1/resources/mcp_servers/" + positional[0]
		if command == "mcp-tools" {
			var response airlockv1.ListIntegrationMCPToolsResponse
			if err := doProto(ctx, baseURL, http.MethodGet, root+"/tools", token, nil, &response); err != nil {
				return err
			}
			return writeProtoJSON(&response)
		}
		if tool == "" {
			return errors.New("mcp-call requires --tool")
		}
		if data == "" {
			data = "{}"
		}
		var response airlockv1.InvokeMCPToolResponse
		if err := doProto(ctx, baseURL, http.MethodPost, root+"/call", token, &airlockv1.InvokeMCPToolRequest{Tool: tool, ArgumentsJson: []byte(data)}, &response); err != nil {
			return err
		}
		if err := writeProtoJSON(&response); err != nil {
			return err
		}
		if response.IsError {
			return errors.New("MCP resource returned a tool error")
		}
		return nil
	default:
		return errors.New("unknown resources command")
	}
}

func resolveUserRemote(ctx context.Context, f targetFlags) (string, string, error) {
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" || os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		return "", "", errors.New("direct resources require a user login for an explicitly selected Airlock remote")
	}
	binding, _, err := loadAgentBinding(f.dir)
	if err != nil {
		return "", "", err
	}
	name := f.remote
	if name == "" {
		name = binding.DefaultRemote
	}
	remote, _ := binding.remote(name)
	baseURL := normalizeBaseURL(f.url)
	if baseURL != "" && remote.AirlockURL != "" && baseURL != normalizeBaseURL(remote.AirlockURL) {
		return "", "", errors.New("--url conflicts with the selected remote")
	}
	if baseURL == "" {
		baseURL = remote.AirlockURL
	}
	if _, err := canonicalAuthOrigin(baseURL); err != nil {
		return "", "", err
	}
	token, err := accessTokenForURL(ctx, baseURL)
	return baseURL, token, err
}
