package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type AgentBudget struct {
	Steps     int64 `json:"steps,omitempty"`
	TimeoutMS int64 `json:"timeoutMs,omitempty"`
	Tokens    int64 `json:"tokens,omitempty"`
}

type AgentDefinition struct {
	Slug                   string                `json:"slug"`
	Description            string                `json:"description"`
	Instructions           string                `json:"instructions"`
	ModelSlot              string                `json:"modelSlot"`
	InputSchema            json.RawMessage       `json:"inputSchema"`
	OutputSchema           json.RawMessage       `json:"outputSchema"`
	ContractHash           string                `json:"contractHash"`
	Tools                  []AgentToolDefinition `json:"tools,omitempty"`
	MCPs                   []string              `json:"mcps,omitempty"`
	Subagents              []string              `json:"subagents,omitempty"`
	Budget                 AgentBudget           `json:"budget"`
	MaxAttempts            int32                 `json:"maxAttempts"`
	MaxConcurrency         int32                 `json:"maxConcurrency"`
	MaxSubagentCalls       int32                 `json:"maxSubagentCalls"`
	MaxConcurrentSubagents int32                 `json:"maxConcurrentSubagents"`
}

type AgentToolDefinition struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	LLMHint       string            `json:"llmHint,omitempty"`
	InputSchema   json.RawMessage   `json:"inputSchema"`
	OutputSchema  json.RawMessage   `json:"outputSchema"`
	InputExamples []json.RawMessage `json:"inputExamples,omitempty"`
}

type AgentReply struct {
	Kind     string          `json:"kind"`
	Output   json.RawMessage `json:"output,omitempty"`
	Question string          `json:"question,omitempty"`
}

type AgentRunInfo struct {
	ID           string      `json:"id"`
	SessionID    string      `json:"sessionId"`
	Definition   string      `json:"definition"`
	ContractHash string      `json:"contractHash"`
	Status       string      `json:"status"`
	Error        string      `json:"error,omitempty"`
	Reply        *AgentReply `json:"reply,omitempty"`
	Steps        int64       `json:"steps"`
	Tokens       int64       `json:"tokens"`
	CreatedAt    time.Time   `json:"createdAt"`
	UpdatedAt    time.Time   `json:"updatedAt"`
	StartedAt    *time.Time  `json:"startedAt,omitempty"`
	CompletedAt  *time.Time  `json:"completedAt,omitempty"`
}

type StartAgentRequest struct {
	RequestID    string          `json:"requestId"`
	ContractHash string          `json:"contractHash"`
	Input        json.RawMessage `json:"input"`
}

type ContinueAgentRequest struct {
	RequestID    string `json:"requestId"`
	ContractHash string `json:"contractHash"`
	Prompt       string `json:"prompt"`
}

type AgentRunResponse struct {
	Run     AgentRunInfo `json:"run"`
	Created bool         `json:"created"`
}

type ListAgentRunsResponse struct {
	Runs       []AgentRunInfo `json:"runs"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

type AgentWaitRequest struct {
	IDs       []string `json:"ids"`
	Mode      string   `json:"mode"`
	TimeoutMS *int64   `json:"timeoutMs,omitempty"`
}

type AgentCallsRequest struct {
	IDs []string `json:"ids"`
}
type AgentWaitResult struct {
	Reason    string         `json:"reason"`
	Completed []AgentRunInfo `json:"completed"`
	Pending   []string       `json:"pending"`
}
type AgentCancelResult struct {
	Calls []AgentRunInfo `json:"calls"`
}

type RuntimeAgentDefinition struct {
	Slug         string `json:"slug"`
	ContractHash string `json:"contractHash"`
}

// AgentDefinitionHash hashes the entire declaration except ContractHash. JSON
// object key order and whitespace are canonicalized; array order is significant.
func AgentDefinitionHash(def AgentDefinition) (string, error) {
	def.ContractHash = ""
	raw, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	delete(value, "contractHash")
	raw, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

var agentIdentifier = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

// ValidateAgentDefinitions validates private contracts and their manifest-local
// references, including exact hashes and a maximum of one child level.
func ValidateAgentDefinitions(manifest AgentManifest) error {
	defs := make(map[string]AgentDefinition, len(manifest.AgentDefinitions))
	models := make(map[string]string, len(manifest.ModelSlots))
	for _, m := range manifest.ModelSlots {
		models[m.Slug] = m.Capability
	}
	mcps := make(map[string]bool, len(manifest.MCPServers))
	for _, m := range manifest.MCPServers {
		mcps[m.Slug] = true
	}
	for _, d := range manifest.AgentDefinitions {
		if !agentIdentifier.MatchString(d.Slug) || len(d.Slug) > 58 {
			return fmt.Errorf("agent %q: invalid slug", d.Slug)
		}
		if _, exists := defs[d.Slug]; exists {
			return fmt.Errorf("agent %q: duplicate definition", d.Slug)
		}
		defs[d.Slug] = d
	}
	for _, d := range manifest.AgentDefinitions {
		fail := func(message string) error { return fmt.Errorf("agent %q: %s", d.Slug, message) }
		if strings.TrimSpace(d.Description) == "" || strings.TrimSpace(d.Instructions) == "" {
			return fail("description and instructions are required")
		}
		if models[d.ModelSlot] != "text" {
			return fail("model slot must reference a registered text model")
		}
		if d.MaxAttempts <= 0 || d.MaxConcurrency <= 0 {
			return fail("maxAttempts and maxConcurrency must be positive")
		}
		b := d.Budget
		if b.Steps < 0 || b.TimeoutMS < 0 || b.Tokens < 0 || b == (AgentBudget{}) {
			return fail("budget must have nonnegative limits and at least one positive limit")
		}
		if len(d.Subagents) == 0 {
			if d.MaxSubagentCalls != 0 || d.MaxConcurrentSubagents != 0 {
				return fail("child limits require subagents")
			}
		} else if d.MaxSubagentCalls <= 0 || d.MaxConcurrentSubagents <= 0 {
			return fail("child limits must be positive")
		}
		for _, schema := range []json.RawMessage{d.InputSchema, d.OutputSchema} {
			if !agentSchema(schema) {
				return fail("input and output schemas must be JSON schema objects or booleans")
			}
		}
		seen := map[string]bool{}
		for _, t := range d.Tools {
			if !agentIdentifier.MatchString(t.Name) || len(t.Name) > 58 || seen[t.Name] {
				return fail("invalid or duplicate private tool name: " + t.Name)
			}
			seen[t.Name] = true
			if strings.TrimSpace(t.Description) == "" || !agentSchema(t.InputSchema) || !agentSchema(t.OutputSchema) {
				return fail("private tool requires description and schemas: " + t.Name)
			}
			for _, example := range t.InputExamples {
				if !json.Valid(example) {
					return fail("invalid private tool input example: " + t.Name)
				}
			}
		}
		seen = map[string]bool{}
		for _, slug := range d.MCPs {
			if !mcps[slug] || seen[slug] {
				return fail("unknown or duplicate MCP: " + slug)
			}
			seen[slug] = true
		}
		seen = map[string]bool{}
		for _, slug := range d.Subagents {
			child, ok := defs[slug]
			if !ok || slug == d.Slug || seen[slug] {
				return fail("unknown, duplicate or self subagent: " + slug)
			}
			if len(child.Subagents) != 0 {
				return fail("subagents may not have children: " + slug)
			}
			seen[slug] = true
		}
		hash, err := AgentDefinitionHash(d)
		if err != nil {
			return err
		}
		if hash != d.ContractHash {
			return fail("contract hash mismatch")
		}
	}
	return nil
}

func agentSchema(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value.(type) {
	case map[string]any, bool:
		return true
	}
	return false
}
