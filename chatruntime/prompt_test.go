package chatruntime_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
)

func TestPromptUsesCanonicalCatalog(t *testing.T) {
	manifest := wire.AgentManifest{Tools: []wire.ToolDef{{
		Name: "lookup", Description: "Look up a record", LLMHint: "Read before writing",
		InputSchema:   json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"note":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["id"]}`),
		OutputSchema:  json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}}}`),
		InputExamples: []json.RawMessage{json.RawMessage(`{"id":"abc"}`)},
	}}, MCPServers: []wire.MCPDef{{Slug: "external", Access: wire.AccessUser}}}
	defs, err := capability.Catalog(manifest, capability.Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{"external": {{Name: "search/issues", InputSchema: json.RawMessage(`{"type":"object"}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := chatruntime.RenderPrompt("You help with records.", defs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You help with records.", "async function body", "run_js is the only provider tool", "call run_js with code such as return await tools.tool_name({...});", "tools.*, conn.*, mcp.*, air.*", "bindings usable only inside run_js code, never provider tool names", "globalThis", "before any JavaScript executes", "declare const tools:", "lookup(args:", "id: string;", "tags?: string[];", "note?: string | null;", "Promise<", "count?: number;", "Read before writing", `await tools.lookup({"id":"abc"})`, "declare const mcp:", "search_issues(args:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q in prompt:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{"ES5", "no async", "last expression", "agent__prompt", "declare const agent:"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("unexpected %q", forbidden)
		}
	}
	direct, err := chatruntime.RenderPrompt("Records", defs, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(direct, "run_js") || strings.Contains(direct, "declare const") {
		t.Fatalf("JS leaked into direct prompt: %s", direct)
	}
	for i, j := 0, len(defs)-1; i < j; i, j = i+1, j-1 {
		defs[i], defs[j] = defs[j], defs[i]
	}
	if got, err := chatruntime.RenderPrompt("You help with records.", defs, false); err != nil || got != prompt {
		t.Fatalf("prompt depends on catalog order: %v", err)
	}
}

func TestPromptExternalMCPSchemas(t *testing.T) {
	input := json.RawMessage(`{"type":"object","$defs":{"Filter":{"oneOf":[{"type":"string"},{"type":"number"},{"type":"null"}]}},"properties":{"filters":{"type":"object","additionalProperties":{"$ref":"#/$defs/Filter"}},"cursor":{"type":["string","null"]}},"required":["filters"],"additionalProperties":false}`)
	manifest := wire.AgentManifest{MCPServers: []wire.MCPDef{{Slug: "external", Access: wire.AccessUser}}}
	defs, err := capability.Catalog(manifest, capability.Discovery{MCPSchemas: map[string][]wire.MCPToolSchema{"external": {{Name: "search", InputSchema: input}}}})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := chatruntime.RenderPrompt("Search", defs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"filters: Record<string, string | number | null>;", "cursor?: string | null;", "declare const mcp:", "declare const user:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q in prompt:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "search(args: {})") {
		t.Fatal("external MCP schema replaced by empty object")
	}
	for _, field := range []string{"input", "output"} {
		t.Run(field, func(t *testing.T) {
			def := capability.Definition{Path: capability.Local(capability.MCP, "external", "search"), Target: capability.Platform, InputSchema: input}
			if field == "input" {
				def.InputSchema = json.RawMessage(`{"type":["string",42]}`)
			} else {
				def.OutputSchema = json.RawMessage(`{"type":"object","additionalProperties":42}`)
			}
			got, err := chatruntime.RenderPrompt("Search", []capability.Definition{def}, false)
			if err == nil || got != "" || !strings.Contains(err.Error(), field+" schema") || !strings.Contains(err.Error(), def.Path.ID()) {
				t.Fatalf("prompt=%q err=%v", got, err)
			}
		})
	}
}
