package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

type resourceAddress struct{ global, target, slot string }

func parseResourceAddress(value string) (resourceAddress, error) {
	if strings.HasPrefix(value, "/") {
		name := strings.TrimPrefix(value, "/")
		if !validRemoteName(name) {
			return resourceAddress{}, errors.New("global resource address requires /name or /UUID without extra path segments")
		}
		return resourceAddress{global: name}, nil
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validRemoteName(parts[0]) || !validRemoteName(parts[1]) {
		return resourceAddress{}, errors.New("resource address requires /global-name or target/slot; bare names are ambiguous")
	}
	if err := requireTarget(parts[0]); err != nil {
		return resourceAddress{}, err
	}
	return resourceAddress{target: parts[0], slot: parts[1]}, nil
}

func selectResourceTarget(f *targetFlags, target string) error {
	if err := requireTarget(target); err != nil {
		return err
	}
	if f.target != "" && f.target != target {
		return errors.New("resource address conflicts with --target")
	}
	f.target = target
	return nil
}

func resolveGlobalResource(ctx context.Context, base, token, selector string) (*airlockv1.OwnedResourceInfo, error) {
	var row airlockv1.OwnedResourceInfo
	if err := doProto(ctx, base, http.MethodGet, "/api/v1/resources/resolve/"+url.PathEscape(selector), token, nil, &row); err != nil {
		return nil, err
	}
	if !deployUUIDRe.MatchString(row.Id) || row.Type == "" {
		return nil, errors.New("invalid resolved resource identity")
	}
	return &row, nil
}

func resourceSlot(ctx context.Context, s selectedDeployment, slug, typ string) (*airlockv1.TargetBindingInfo, error) {
	var rows airlockv1.ListTargetBindingsResponse
	if err := doProto(ctx, s.baseURL, http.MethodGet, s.path("/bindings"), s.token, nil, &rows); err != nil {
		return nil, err
	}
	if rows.RuntimeAgentId != s.info.Id {
		return nil, errors.New("binding inventory selected another runtime")
	}
	var selected *airlockv1.TargetBindingInfo
	for _, row := range rows.Bindings {
		if row.Slug != slug || typ != "" && row.Type != typ {
			continue
		}
		if selected != nil {
			return nil, errors.New("slot name has multiple resource types; specify --type")
		}
		selected = row
	}
	if selected == nil {
		return nil, errors.New("resource slot is not declared on the selected target")
	}
	return selected, nil
}

func printResourceMCPTools(response *airlockv1.ListIntegrationMCPToolsResponse) error {
	type tool struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	}
	out := struct {
		Instructions string `json:"instructions,omitempty"`
		Tools        []tool `json:"tools"`
	}{Instructions: response.Instructions, Tools: make([]tool, 0, len(response.Tools))}
	for _, row := range response.Tools {
		input := json.RawMessage(row.InputSchemaJson)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, tool{row.Name, row.Description, input, row.OutputSchemaJson})
	}
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

// Codegen is bound to one runtime UUID, not a user resource inventory or a
// selectable deployment. Its address uses that exact injected runtime UUID.
func codegenResourceCommand(ctx context.Context, command string, a resourceAddress, typ string, f targetFlags, path, method, data, tool string, headers map[string]string) error {
	if f.url != "" || f.remote != "" || f.agent != "" {
		return errors.New("codegen resources are fixed to the injected runtime")
	}
	t, err := resolveIntegrationTarget(ctx, integrationTargetFlags{})
	if err != nil {
		return err
	}
	if command == "list" || command == "bindings" {
		if f.target != "" && f.target != t.agentID {
			return errors.New("codegen target conflicts with the injected runtime")
		}
		var out airlockv1.ListIntegrationsResponse
		if err := doProto(ctx, t.baseURL, "GET", t.path("/"), t.token, nil, &out); err != nil {
			return err
		}
		return writeProtoJSON(&out)
	}
	if a.target != os.Getenv("AIRLOCK_AGENT_ID") || a.global != "" {
		return errors.New("codegen resource address must be <injected-runtime-UUID>/<slot>; global resources require developer/user authority")
	}
	var list airlockv1.ListIntegrationsResponse
	if err := doProto(ctx, t.baseURL, "GET", t.path("/"), t.token, nil, &list); err != nil {
		return err
	}
	var slot *airlockv1.IntegrationInfo
	for _, row := range list.Integrations {
		if row.Slug == a.slot && (typ == "" || typ == row.Type) {
			if slot != nil {
				return errors.New("ambiguous slot; specify --type")
			}
			slot = row
		}
	}
	if slot == nil {
		return errors.New("resource slot is not declared on the bound runtime")
	}
	if command == "inspect" {
		return writeProtoJSON(slot)
	}
	if command == "request" {
		if slot.Type != "connection" || path == "" {
			return errors.New("request requires a connection slot and --path")
		}
		var out airlockv1.InvokeConnectionResponse
		if err := doProto(ctx, t.baseURL, "POST", t.path("/connections/"+url.PathEscape(a.slot)+"/request"), t.token, &airlockv1.InvokeConnectionRequest{Method: method, Path: path, Body: []byte(data), Headers: headers}, &out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "HTTP %d\n", out.StatusCode)
		if _, err := os.Stdout.Write(out.Body); err != nil {
			return err
		}
		if out.StatusCode >= 400 {
			return fmt.Errorf("resource returned HTTP %d", out.StatusCode)
		}
		return nil
	}
	if slot.Type != "mcp_server" {
		return errors.New("MCP operation requires an MCP slot")
	}
	root := t.path("/mcp/" + url.PathEscape(a.slot))
	if command == "mcp-tools" {
		var out airlockv1.ListIntegrationMCPToolsResponse
		if err := doProto(ctx, t.baseURL, "GET", root+"/tools", t.token, nil, &out); err != nil {
			return err
		}
		return printResourceMCPTools(&out)
	}
	if command != "mcp-call" || tool == "" {
		return errors.New("codegen resources support list, inspect, request, mcp-tools and mcp-call")
	}
	if data == "" {
		data = "{}"
	}
	var out airlockv1.InvokeMCPToolResponse
	if err := doProto(ctx, t.baseURL, "POST", root+"/call", t.token, &airlockv1.InvokeMCPToolRequest{Tool: tool, ArgumentsJson: []byte(data)}, &out); err != nil {
		return err
	}
	if err := writeProtoJSON(&out); err != nil {
		return err
	}
	if out.IsError {
		return errors.New("MCP resource returned a tool error")
	}
	return nil
}
