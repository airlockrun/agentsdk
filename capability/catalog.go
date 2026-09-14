package capability

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
)

type Target string

const (
	App      Target = "app"
	Platform Target = "platform"
	// Executor is a JS intrinsic, not an app or platform RPC.
	Executor Target = "executor"
)

// Definition is a capability contract, not a grant. The broker intersects the
// catalogue with the authenticated run's policy before exposing either alias.
type Definition struct {
	Path         Path
	Target       Target
	Access       wire.Access
	Description  string
	LLMHint      string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	// TextOutput marks plain-text tool results that must remain strings in JS,
	// even when their contents happen to be valid JSON (for example file_read).
	TextOutput    bool
	InputExamples []json.RawMessage
}

// Discovery contains platform-resolved schemas for declared external MCP servers.
type Discovery struct {
	MCPSchemas map[string][]wire.MCPToolSchema
}

// Fixed returns the complete fixed inventory, never a caller-selected subset.
// Directory-specific read/write/list checks still apply at execution time.
func Fixed() []Definition {
	var defs []Definition
	for _, fixed := range []struct {
		op          string
		target      Target
		access      wire.Access
		description string
		input       any
	}{
		{"file_read", App, wire.AccessPublic, "Read stored UTF-8 text, capped at 16 MiB. Use line or range operations for larger files.", PathInput{}},
		{"file_read_bytes", App, wire.AccessPublic, "Read raw file bytes as {size, base64}, capped at 16 MiB.", PathInput{}},
		{"file_read_range_bytes", App, wire.AccessPublic, "Read an exact byte window as {size, base64}.", FileReadRangeInput{}},
		{"file_grep", App, wire.AccessPublic, "Stream-grep a file with an RE2 pattern. Output is bounded.", FileGrepInput{}},
		{"file_head", App, wire.AccessPublic, "Read the first N lines (default 10).", FileNLinesInput{}},
		{"file_tail", App, wire.AccessPublic, "Read the last N lines (default 10), fetching only a trailing window.", FileNLinesInput{}},
		{"file_lines", App, wire.AccessPublic, "Read a 1-based line window.", FileLinesInput{}},
		{"file_stat", App, wire.AccessPublic, "Return stored file metadata.", PathInput{}},
		{"file_exists", App, wire.AccessPublic, "Check whether an accessible stored file exists.", PathInput{}},
		{"file_write", App, wire.AccessPublic, "Write text or base64 bytes and return file metadata.", FileWriteInput{}},
		{"file_delete", App, wire.AccessPublic, "Delete a stored file.", PathInput{}},
		{"file_list", App, wire.AccessPublic, "List a storage directory, optionally recursively.", FileListInput{}},
		{"file_encode", App, wire.AccessUser, "Encode a file using base64, base64url, hex, or gzip. Omit dst for scratch storage.", TransformInput{}},
		{"file_decode", App, wire.AccessUser, "Decode a file using base64, base64url, hex, or gzip.", TransformInput{}},
		{"file_decode_text", App, wire.AccessUser, "Decode a named charset into UTF-8 text.", TransformInput{}},
		{"file_edit_lines", App, wire.AccessUser, "Apply streaming line edits. Pass src as dst for in-place editing.", FileEditLinesInput{}},
		{"file_sed", App, wire.AccessUser, "Apply a streaming sed-subset script.", FileSedInput{}},
		{"query_db", App, wire.AccessAdmin, "Query the app database inside a read-only transaction; returns row objects.", QueryDBInput{}},
		{"output", Platform, wire.AccessPublic, "Share media with the bound conversation. Prose belongs in the normal reply.", OutputInput{}},
		{"file_share_url", Platform, wire.AccessPublic, "Mint a presigned, unauthenticated file URL, capped at 24 hours.", FileShareURLInput{}},
		{"http_request", Platform, wire.AccessUser, "Request a URL through the platform egress proxy.", HTTPRequestInput{}},
		{"web_search", Platform, wire.AccessUser, "Search the web through the platform search provider.", WebSearchInput{}},
		{"attach_to_context", Platform, wire.AccessUser, "Attach a stored file to the next model turn by reference; idempotent per run.", PathInput{}},
		{"analyze_image", Platform, wire.AccessUser, "Ask the platform vision model about a stored image.", AnalyzeImageInput{}},
		{"transcribe_audio", Platform, wire.AccessUser, "Transcribe stored audio through the platform transcription model.", TranscribeAudioInput{}},
		{"generate_image", Platform, wire.AccessUser, "Generate an image and store it; returns {file, mimeType, size}.", GenerateImageInput{}},
		{"speak", Platform, wire.AccessUser, "Synthesize speech and store it; returns {file, mimeType, size}.", SpeakInput{}},
		{"embed", Platform, wire.AccessUser, "Embed text through the platform embedding model.", EmbedInput{}},
		{"request_upgrade", Platform, wire.AccessAdmin, "Request a platform-side app upgrade.", RequestUpgradeInput{}},
	} {
		d := Definition{Path: Local(Air, "", fixed.op), Target: fixed.target, Access: fixed.access,
			Description: fixed.description, InputSchema: schema.MustFromType(fixed.input).MustJSON()}
		switch fixed.op {
		case "file_read", "file_grep", "file_head", "file_tail", "file_lines":
			d.TextOutput = true
			d.OutputSchema = json.RawMessage(`{"type":"string"}`)
		}
		defs = append(defs, d)
	}
	defs = append(defs,
		Definition{Path: Local(Air, "", "log"), Target: Executor, Access: wire.AccessPublic, Description: "Append a bounded JS log entry; console methods are executor intrinsics.", InputSchema: json.RawMessage(`{"type":"array","items":{}}`)},
	)
	return defs
}

// Catalog constructs identities from declarations, never presentation aliases.
// It rejects collisions across the entire catalogue before policy selection so
// selecting a subset cannot hide a conflicting definition and escalate access.
// Declared tools retain their manifest schemas and examples verbatim.
func Catalog(manifest wire.AgentManifest, discovery Discovery) ([]Definition, error) {
	defs := Fixed()
	for _, t := range manifest.Tools {
		defs = append(defs, Definition{Path: Local(Tool, "", t.Name), Target: App, Access: t.Access,
			Description: t.Description, LLMHint: t.LLMHint, InputSchema: t.InputSchema,
			OutputSchema: t.OutputSchema, InputExamples: t.InputExamples})
	}
	for _, c := range manifest.Connections {
		for _, op := range []string{"request", "request_json"} {
			defs = append(defs, Definition{Path: Local(Connection, c.Slug, op), Target: Platform, Access: c.Access, Description: c.Description, LLMHint: c.LLMHint, InputSchema: schema.MustFromType(ConnectionRequestInput{}).MustJSON()})
		}
	}
	for _, t := range manifest.Topics {
		for _, op := range []string{"subscribe", "unsubscribe"} {
			defs = append(defs, Definition{Path: Local(Topic, t.Slug, op), Target: Platform, Access: t.Access, Description: t.Description, LLMHint: t.LLMHint, InputSchema: schema.MustFromType(struct{}{}).MustJSON()})
		}
	}
	addExternal := func(kind Kind, namespace, alias string, access wire.Access, tools []wire.MCPToolSchema) error {
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.Name
		}
		paths, err := External(kind, namespace, alias, names)
		if err != nil {
			return err
		}
		for _, t := range tools {
			defs = append(defs, Definition{Path: paths[t.Name], Target: Platform, Access: access, Description: t.Description, InputSchema: t.InputSchema})
		}
		return nil
	}
	for _, m := range manifest.MCPServers {
		if m.Access == "" {
			continue
		}
		if err := addExternal(MCP, m.Slug, m.Slug, m.Access, discovery.MCPSchemas[m.Slug]); err != nil {
			return nil, err
		}
	}
	ids, direct, js := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range defs {
		d := &defs[i]
		if d.Access == "" {
			d.Access = wire.AccessUser
		}
		if d.Access != wire.AccessPublic && d.Access != wire.AccessUser && d.Access != wire.AccessAdmin {
			return nil, fmt.Errorf("invalid access for %s", d.Path.ID())
		}
		if d.Path.CanonicalOperation() == "" || ids[d.Path.ID()] || direct[d.Path.Direct()] || (d.Path.JS() != "" && js[d.Path.JS()]) {
			return nil, fmt.Errorf("duplicate or invalid capability: %s", d.Path.ID())
		}
		ids[d.Path.ID()], direct[d.Path.Direct()] = true, true
		if d.Path.JS() != "" {
			js[d.Path.JS()] = true
		}
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Path.ID() < defs[j].Path.ID() })
	return defs, nil
}

// DefinitionCatalog constructs the private tool and bound MCP inventory for an
// exact task contract. It excludes global app tools and unbound MCPs. Fixed
// platform operations, connections and topics retain their normal access rules.
// The host must authenticate scope from a persisted application-owned run.
func DefinitionCatalog(manifest wire.AgentManifest, scope wire.RuntimeAgentDefinition, discovery Discovery) ([]Definition, error) {
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		return nil, err
	}
	var selected *wire.AgentDefinition
	for i := range manifest.AgentDefinitions {
		d := &manifest.AgentDefinitions[i]
		if d.Slug == scope.Slug && d.ContractHash == scope.ContractHash {
			selected = d
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("unknown agent definition contract %q", scope.Slug)
	}
	manifest.Tools = nil
	for _, t := range selected.Tools {
		manifest.Tools = append(manifest.Tools, wire.ToolDef{Name: t.Name, Description: t.Description, LLMHint: t.LLMHint,
			Access: wire.AccessAdmin, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema, InputExamples: t.InputExamples})
	}
	bound := make(map[string]bool, len(selected.MCPs))
	for _, slug := range selected.MCPs {
		bound[slug] = true
	}
	servers := manifest.MCPServers
	manifest.MCPServers = nil
	for _, m := range servers {
		if bound[m.Slug] {
			m.Access = wire.AccessAdmin
			manifest.MCPServers = append(manifest.MCPServers, m)
		}
	}
	return Catalog(manifest, discovery)
}
