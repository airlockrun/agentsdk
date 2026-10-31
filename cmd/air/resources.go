package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/proto"
)

// Resource inventory/direct use is user+remote scoped. Binding commands also
// require app+target; neither operation establishes a developer session.
func cmdResources(args []string) error {
	if len(args) == 0 {
		return errors.New("resources requires list, inspect, request, mcp-tools, mcp-call, mcp-probe, bindings, bind, fixture or unbind")
	}
	command := args[0]
	if command == "mcp-probe" {
		if len(args) != 2 {
			return errors.New("resources mcp-probe requires one HTTP(S) URL")
		}
		return probeMCP(args[1])
	}
	resource, fixture, revision, data, method, path, tool := "", "", "", "", "GET", "", ""
	typ := ""
	headers := map[string]string{}
	var targetArgs []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--resource", "--fixture", "--revision", "--data", "--method", "--path", "--tool", "--type", "--header":
			key := args[i]
			allowed := key == "--type" || key == "--resource" && command == "bind" || (key == "--fixture" || key == "--revision") && command == "fixture" || key == "--data" && (command == "request" || command == "mcp-call") || (key == "--method" || key == "--path" || key == "--header") && command == "request" || key == "--tool" && command == "mcp-call"
			if !allowed {
				return fmt.Errorf("%s is not supported by resources %s; pending candidate selections use preview/deploy --bind", key, command)
			}
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", key)
			}
			i++
			switch key {
			case "--header":
				name, value, ok := strings.Cut(args[i], ":")
				if !ok || strings.TrimSpace(name) == "" {
					return errors.New("--header requires name:value")
				}
				headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
			case "--type":
				typ = args[i]
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
	f, positional, err := parseSelectionFlags(targetArgs)
	if err != nil {
		return err
	}
	if f.wait {
		return errors.New("resource commands do not support --wait")
	}
	if command == "list" && (len(positional) == 1 || f.target != "") {
		if len(positional) > 0 {
			if err := selectResourceTarget(&f, positional[0]); err != nil {
				return err
			}
			positional = nil
		}
		command = "bindings"
		isBinding = true
	}
	if command == "list" && f.agent != "" {
		return errors.New("global resources are user+remote scoped; --agent is unavailable")
	}
	if (command == "list" || command == "bindings") && len(positional) != 0 {
		return errors.New("resources list takes at most one target; bindings takes no positional arguments")
	}
	var address resourceAddress
	if command != "list" && command != "bindings" {
		if len(positional) != 1 {
			return errors.New("resource operation requires /global-name or target/slot; use --type only to disambiguate a slot")
		}
		address, err = parseResourceAddress(positional[0])
		if err != nil {
			return err
		}
		if address.target != "" {
			if err := selectResourceTarget(&f, address.target); err != nil {
				return err
			}
		} else if f.target != "" || f.agent != "" {
			return errors.New("global resources are user+remote scoped; --agent and --target are unavailable")
		}
	}
	if isBinding {
		if err := requireTarget(f.target); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return codegenResourceCommand(ctx, command, address, typ, f, path, method, data, tool, headers)
	}
	if isBinding {
		if command != "bindings" && address.target == "" {
			return errors.New("binding operations require target/slot")
		}
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
		if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" && f.target == "production" {
			return errors.New("production binding changes require an approved immutable deployment candidate; prepare selections with deploy --bind/--fixture, not a production execution grant")
		}
		slot, err := resourceSlot(ctx, s, address.slot, typ)
		if err != nil {
			return err
		}
		var input proto.Message
		verb := http.MethodPut
		switch command {
		case "bind":
			selected, e := parseResourceAddress(resource)
			if e != nil || selected.target != "" || fixture != "" {
				return errors.New("bind requires --resource /global-name or /UUID, without --fixture")
			}
			resolved, e := resolveGlobalResource(ctx, s.baseURL, s.token, selected.global)
			if e != nil {
				return e
			}
			if resolved.Type != slot.Type {
				return errors.New("resource type does not match the declared slot")
			}
			input = &airlockv1.SetTargetBindingRequest{ResourceId: resolved.Id}
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
		if err := doProto(ctx, s.baseURL, verb, s.path("/bindings/"+url.PathEscape(slot.Type)+"/"+url.PathEscape(address.slot)), s.token, input, &response); err != nil {
			return err
		}
		if response.RuntimeAgentId != s.info.Id {
			return errors.New("binding response selected a different runtime")
		}
		return writeProtoJSON(&response)
	}
	var baseURL, token, requestPath, mcpRoot string
	if address.target != "" {
		s, e := resolveDeployment(ctx, f, false)
		if e != nil {
			return e
		}
		slot, e := resourceSlot(ctx, s, address.slot, typ)
		if e != nil {
			return e
		}
		if command == "inspect" {
			return writeProtoJSON(slot)
		}
		if (command == "request" && slot.Type != "connection") || ((command == "mcp-tools" || command == "mcp-call") && slot.Type != "mcp_server") {
			return errors.New("operation does not match the declared slot type")
		}
		baseURL, token = s.baseURL, s.token
		requestPath = s.path("/resources/connections/" + url.PathEscape(address.slot) + "/request")
		mcpRoot = s.path("/resources/mcp/" + url.PathEscape(address.slot))
	} else {
		baseURL, token, err = resolveUserRemote(ctx, f)
		if err != nil {
			return err
		}
		if command != "list" {
			resolved, e := resolveGlobalResource(ctx, baseURL, token, address.global)
			if e != nil {
				return e
			}
			if typ != "" && typ != resolved.Type {
				return errors.New("resource type does not match --type")
			}
			if command == "inspect" {
				positional = []string{resolved.Type, resolved.Id}
			} else {
				positional = []string{resolved.Id}
			}
			if (command == "request" && resolved.Type != "connection") || ((command == "mcp-tools" || command == "mcp-call") && resolved.Type != "mcp_server") {
				return errors.New("operation does not match the resource type")
			}
			requestPath = "/api/v1/resources/connections/" + resolved.Id + "/request"
			mcpRoot = "/api/v1/resources/mcp_servers/" + resolved.Id
		}
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
		if path == "" {
			return errors.New("resources request requires an address and --path /path")
		}
		var response airlockv1.InvokeConnectionResponse
		if err := doProto(ctx, baseURL, http.MethodPost, requestPath, token, &airlockv1.InvokeConnectionRequest{Method: method, Path: path, Body: []byte(data), Headers: headers}, &response); err != nil {
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
		root := mcpRoot
		if command == "mcp-tools" {
			var response airlockv1.ListIntegrationMCPToolsResponse
			if err := doProto(ctx, baseURL, http.MethodGet, root+"/tools", token, nil, &response); err != nil {
				return err
			}
			return printResourceMCPTools(&response)
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
	if os.Getenv("AIRLOCK_INTEGRATION_TOKEN") != "" {
		return "", "", errors.New("build integration credentials cannot access direct resources")
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
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		injectedURL := normalizeBaseURL(os.Getenv("AIRLOCK_API_URL"))
		injectedOrigin, err := canonicalAuthOrigin(injectedURL)
		if err != nil {
			return "", "", errors.New("developer session requires a valid AIRLOCK_API_URL")
		}
		for _, selected := range []string{baseURL, remote.AirlockURL} {
			if selected == "" {
				continue
			}
			origin, err := canonicalAuthOrigin(selected)
			if err != nil || origin != injectedOrigin {
				return "", "", errors.New("selected resource remote conflicts with the admitted developer host")
			}
		}
		if f.remote != "" && remote.AirlockURL == "" {
			return "", "", errors.New("selected resource remote is not configured")
		}
		token, err := accessTokenForURL(ctx, injectedURL)
		return injectedURL, token, err
	}
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
