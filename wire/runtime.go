package wire

import (
	"encoding/json"
	"fmt"
)

// AppRuntimeProtocol identifies the app capability execution contract, independent
// of SDK semver and the JavaScript executor's framed protocol.
const AppRuntimeProtocol = "airlock.app-runtime.v2"

func CheckAppRuntimeProtocol(reported string) error {
	if reported != AppRuntimeProtocol {
		return fmt.Errorf("app runtime protocol %q is incompatible with required %q; rebuild the agent", reported, AppRuntimeProtocol)
	}
	return nil
}

const RuntimeInvokePath = "/__air/runtime/invoke"

const InvocationTokenHeader = "X-Airlock-Invocation-Token"

const TestExecutorContentType = "application/vnd.airlock.jsexec"

// RuntimeInvokeRequest is an internal broker-to-app call, authenticated with the
// target agent's bearer token. The broker must load the active run from shared
// storage, verify its agent ownership and authorize the selected capability
// before constructing this request. Never populate Context from public headers
// or accept a caller-supplied catalogue as an authorization source.
type RuntimeInvokeRequest struct {
	RuntimeProtocol string          `json:"runtimeProtocol"`
	CapabilityID    string          `json:"capabilityId"`
	ToolCallID      string          `json:"toolCallId"`
	Input           json.RawMessage `json:"input"`
	Context         RuntimeContext  `json:"context"`
}

// RuntimeContext borrows an existing same-agent run. The app must not create or
// complete it. All attribution is supplied by the authenticated broker.
type RuntimeContext struct {
	AgentID             string                  `json:"agentId"`
	RunID               string                  `json:"runId"`
	InvocationToken     string                  `json:"invocationToken"`
	BridgeID            string                  `json:"bridgeId,omitempty"`
	ConversationID      string                  `json:"conversationId,omitempty"`
	Caller              Caller                  `json:"caller"`
	SupportedModalities []string                `json:"supportedModalities,omitempty"`
	Job                 *RuntimeJobContext      `json:"job,omitempty"`
	Definition          *RuntimeAgentDefinition `json:"definition,omitempty"`
}

type RuntimeJobContext struct {
	ID         string `json:"id"`
	Attempt    int    `json:"attempt"`
	LeaseToken string `json:"leaseToken"`
}

// RuntimeAttachment references agent object storage, never inline file bytes.
type RuntimeAttachment struct {
	Path     string `json:"path"`
	MimeType string `json:"mimeType"`
	Filename string `json:"filename,omitempty"`
}

// RuntimeInvokeResponse retains the complete tool envelope and invocation-local
// telemetry. Airlock merges Actions and Logs into the borrowed run's record.
// Tool failures use Error with HTTP 200; protocol/auth failures use non-2xx.
type RuntimeInvokeResponse struct {
	RuntimeProtocol string              `json:"runtimeProtocol"`
	Output          string              `json:"output"`
	Title           string              `json:"title,omitempty"`
	Metadata        map[string]any      `json:"metadata,omitempty"`
	Warnings        []string            `json:"warnings,omitempty"`
	Attachments     []RuntimeAttachment `json:"attachments,omitempty"`
	Logs            []LogEntry          `json:"logs,omitempty"`
	Actions         []Action            `json:"actions,omitempty"`
	Error           string              `json:"error,omitempty"`
}
