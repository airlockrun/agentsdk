package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector"
	"github.com/airlockrun/agentsdk/connector/protocol"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

// Connector declares the complete subset of a connector contract required by
// an agent. Multiple asks Airlock to bind a target set rather than one resource.
type Connector struct {
	noUnkeyedLiterals
	Slug        string
	Description string
	Requires    protocol.Requirement
	Multiple    bool
}

type ConnectorHandle struct {
	slug        string
	contractID  string
	commands    map[string]protocol.CommandDescriptor
	directories map[string]protocol.DirectoryDescriptor
	agent       *Agent
}

func (a *Agent) RegisterConnector(value *Connector) *ConnectorHandle {
	done := a.beginRegistration("RegisterConnector")
	defer done()
	if value == nil {
		panic("agentsdk: RegisterConnector: nil *Connector")
	}
	copy := *value
	copy.Requires = cloneConnectorRequirement(value.Requires)
	sort.Slice(copy.Requires.Commands, func(i, j int) bool { return copy.Requires.Commands[i].Name < copy.Requires.Commands[j].Name })
	sort.Slice(copy.Requires.Directories, func(i, j int) bool { return copy.Requires.Directories[i].Name < copy.Requires.Directories[j].Name })
	if !localIdentifierPattern.MatchString(copy.Slug) {
		panic("agentsdk: RegisterConnector: Slug must be a lowercase underscore identifier")
	}
	if strings.TrimSpace(copy.Description) == "" {
		panic("agentsdk: RegisterConnector: Description is required")
	}
	if err := protocol.ValidateRequirement(copy.Requires); err != nil {
		panic("agentsdk: RegisterConnector: " + err.Error())
	}
	if _, exists := a.connectors[copy.Slug]; exists {
		panic("agentsdk: duplicate RegisterConnector: " + copy.Slug)
	}
	a.connectors[copy.Slug] = &copy
	handle := &ConnectorHandle{
		slug: copy.Slug, contractID: copy.Requires.ContractID,
		commands: make(map[string]protocol.CommandDescriptor), directories: make(map[string]protocol.DirectoryDescriptor), agent: a,
	}
	for _, command := range copy.Requires.Commands {
		handle.commands[command.Name] = command
	}
	for _, directory := range copy.Requires.Directories {
		handle.directories[directory.Name] = directory
	}
	return handle
}

// CallConnector invokes a unary typed command through a registered need. Go
// does not support generic methods, so the typed call is a package function.
func CallConnector[In, Out any](ctx context.Context, handle *ConnectorHandle, command connector.Command[In, Out], input In) (Out, error) {
	var output Out
	if handle == nil || handle.agent == nil {
		return output, errors.New("agentsdk: connector handle is required")
	}
	if !handle.agent.runtimeAvailable() {
		return output, handle.agent.runtimeUnavailable("CallConnector")
	}
	descriptor, exists := handle.commands[command.Name()]
	wanted := command.Descriptor()
	if !exists || command.ContractID() != handle.contractID || !sameCommandContract(descriptor, wanted) || wanted.Mode != protocol.CommandModeUnary {
		return output, fmt.Errorf("agentsdk: unary connector command %s is not declared by need %s", command.Name(), handle.slug)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return output, fmt.Errorf("agentsdk: marshal connector command input: %w", err)
	}
	if len(raw) > protocol.MaxJobPayloadBytes {
		return output, fmt.Errorf("agentsdk: connector command input exceeds %d bytes", protocol.MaxJobPayloadBytes)
	}
	request := protocol.CommandCallRequest{Revision: wanted.Revision, Mode: wanted.Mode, InputSchemaHash: wanted.InputSchemaHash, OutputSchemaHash: wanted.OutputSchemaHash, Input: raw}
	if deadline, ok := ctx.Deadline(); ok {
		request.Deadline = &deadline
	}
	var response protocol.CommandCallResponse
	endpoint := "/api/agent/connectors/" + url.PathEscape(handle.slug) + "/commands/" + url.PathEscape(command.Name())
	if err := handle.agent.client.doJSON(ctx, http.MethodPost, endpoint, request, &response); err != nil {
		return output, err
	}
	if response.Status != "success" {
		return output, fmt.Errorf("agentsdk: connector job %s %s: %s", response.JobID, response.Status, response.Error)
	}
	if err := strictDecodeConnector(response.Output, &output); err != nil {
		return output, fmt.Errorf("agentsdk: decode connector output: %w", err)
	}
	return output, nil
}

type ConnectorJobHandle[Out any] struct {
	agent   *Agent
	need    string
	id      string
	command protocol.CommandDescriptor
}

type ConnectorJobResult[Out any] struct {
	Output Out
	Info   wire.ConnectorJobInfo
}

func StartConnectorJob[In, Out any](ctx context.Context, handle *ConnectorHandle, requestID uuid.UUID, command connector.Command[In, Out], input In) (*ConnectorJobHandle[Out], error) {
	if requestID == uuid.Nil {
		return nil, errors.New("agentsdk: StartConnectorJob requires a caller-generated request UUID")
	}
	if command.Mode() != protocol.CommandModeJob {
		return nil, errors.New("agentsdk: StartConnectorJob requires a job-mode command")
	}
	if handle == nil || handle.agent == nil || !handle.agent.runtimeAvailable() {
		return nil, errors.New("agentsdk: connector runtime is unavailable")
	}
	descriptor, exists := handle.commands[command.Name()]
	if !exists || command.ContractID() != handle.contractID || !sameCommandContract(descriptor, command.Descriptor()) {
		return nil, errors.New("agentsdk: connector job is not declared by the need")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(raw) > protocol.MaxJobPayloadBytes {
		return nil, fmt.Errorf("agentsdk: connector job input exceeds %d bytes", protocol.MaxJobPayloadBytes)
	}
	request := protocol.CommandCallRequest{RequestID: requestID.String(), Revision: descriptor.Revision, Mode: descriptor.Mode, InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: raw}
	if deadline, ok := ctx.Deadline(); ok {
		request.Deadline = &deadline
	}
	var response protocol.CommandCallResponse
	endpoint := "/api/agent/connectors/" + url.PathEscape(handle.slug) + "/jobs/" + url.PathEscape(command.Name())
	if err := handle.agent.client.doJSON(ctx, http.MethodPost, endpoint, request, &response); err != nil {
		return nil, err
	}
	if response.JobID == "" {
		return nil, errors.New("agentsdk: Airlock returned an empty connector job ID")
	}
	return &ConnectorJobHandle[Out]{agent: handle.agent, need: handle.slug, id: response.JobID, command: descriptor}, nil
}

func (h *ConnectorJobHandle[Out]) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

func (h *ConnectorJobHandle[Out]) Get(ctx context.Context) (wire.ConnectorJobInfo, error) {
	var result wire.ConnectorJobInfo
	if h == nil || h.agent == nil {
		return result, errors.New("agentsdk: connector job handle is required")
	}
	err := h.agent.client.doJSON(ctx, http.MethodGet, "/api/agent/connectors/"+url.PathEscape(h.need)+"/jobs/"+url.PathEscape(h.id), nil, &result)
	return result, err
}

func (h *ConnectorJobHandle[Out]) Wait(ctx context.Context) (ConnectorJobResult[Out], error) {
	var result ConnectorJobResult[Out]
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := h.Get(ctx)
		if err != nil {
			return result, err
		}
		result.Info = info
		switch info.Status {
		case "success":
			if err := strictDecodeConnector(info.Output, &result.Output); err != nil {
				return result, err
			}
			return result, nil
		case "error", "canceled", "timeout":
			return result, fmt.Errorf("agentsdk: connector job %s %s: %s", h.id, info.Status, info.Error)
		}
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = h.Cancel(cancelCtx)
			cancel()
			return result, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *ConnectorJobHandle[Out]) Cancel(ctx context.Context) error {
	if h == nil || h.agent == nil {
		return errors.New("agentsdk: connector job handle is required")
	}
	return h.agent.client.doJSON(ctx, http.MethodDelete, "/api/agent/connectors/"+url.PathEscape(h.need)+"/jobs/"+url.PathEscape(h.id), nil, nil)
}

func (h *ConnectorHandle) List(ctx context.Context, directory connector.Directory, request protocol.DirectoryListRequest) (protocol.DirectoryListResponse, error) {
	var result protocol.DirectoryListResponse
	query := url.Values{"path": []string{request.Path}}
	if request.Cursor != "" {
		query.Set("cursor", request.Cursor)
	}
	if request.Limit != 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	err := h.directoryGET(ctx, directory, "list", query, &result)
	return result, err
}

func (h *ConnectorHandle) Stat(ctx context.Context, directory connector.Directory, filePath string) (protocol.DirectoryEntry, error) {
	var result protocol.DirectoryEntry
	err := h.directoryGET(ctx, directory, "stat", url.Values{"path": []string{filePath}}, &result)
	return result, err
}

func (h *ConnectorHandle) Read(ctx context.Context, directory connector.Directory, request protocol.DirectoryReadRequest) (protocol.DirectoryReadResponse, error) {
	var result protocol.DirectoryReadResponse
	query := url.Values{"path": []string{request.Path}}
	if request.Offset != 0 {
		query.Set("offset", strconv.FormatInt(request.Offset, 10))
	}
	if request.Length != 0 {
		query.Set("length", strconv.FormatInt(request.Length, 10))
	}
	err := h.directoryGET(ctx, directory, "read", query, &result)
	return result, err
}

func (h *ConnectorHandle) Write(ctx context.Context, directory connector.Directory, request protocol.DirectoryWriteRequest) (protocol.DirectoryEntry, error) {
	var result protocol.DirectoryEntry
	err := h.directoryCall(ctx, directory, "write", request, &result)
	return result, err
}

func (h *ConnectorHandle) Delete(ctx context.Context, directory connector.Directory, filePath string) error {
	return h.directoryCallMethod(ctx, directory, "delete", http.MethodDelete, struct {
		Path string `json:"path"`
	}{filePath}, nil)
}

func (h *ConnectorHandle) Move(ctx context.Context, directory connector.Directory, request protocol.DirectoryMoveRequest) error {
	return h.directoryCall(ctx, directory, "move", request, nil)
}

type ConnectorImportRequest struct {
	Source    FilePath `json:"source"`
	Path      string   `json:"path"`
	Overwrite bool     `json:"overwrite"`
}

func (h *ConnectorHandle) Import(ctx context.Context, directory connector.Directory, request ConnectorImportRequest) (protocol.DirectoryEntry, error) {
	var result protocol.DirectoryEntry
	err := h.directoryCall(ctx, directory, "import", request, &result)
	return result, err
}

type ConnectorExportRequest struct {
	Path        string   `json:"path"`
	Destination FilePath `json:"destination"`
}

func (h *ConnectorHandle) Export(ctx context.Context, directory connector.Directory, request ConnectorExportRequest) (FileInfo, error) {
	var result FileInfo
	err := h.directoryCall(ctx, directory, "export", request, &result)
	return result, err
}

func (h *ConnectorHandle) directoryCall(ctx context.Context, directory connector.Directory, operation string, request, result any) error {
	return h.directoryCallMethod(ctx, directory, operation, http.MethodPost, request, result)
}

func (h *ConnectorHandle) directoryCallMethod(ctx context.Context, directory connector.Directory, operation, method string, request, result any) error {
	if err := h.validateDirectoryOperation(directory, operation); err != nil {
		return err
	}
	endpoint := "/api/agent/connectors/" + url.PathEscape(h.slug) + "/directories/" + url.PathEscape(directory.Name()) + "/" + operation
	return h.agent.client.doJSON(ctx, method, endpoint, request, result)
}

func (h *ConnectorHandle) directoryGET(ctx context.Context, directory connector.Directory, operation string, query url.Values, result any) error {
	if err := h.validateDirectoryOperation(directory, operation); err != nil {
		return err
	}
	endpoint := "/api/agent/connectors/" + url.PathEscape(h.slug) + "/directories/" + url.PathEscape(directory.Name()) + "/" + operation + "?" + query.Encode()
	return h.agent.client.doJSON(ctx, http.MethodGet, endpoint, nil, result)
}

func (h *ConnectorHandle) validateDirectoryOperation(directory connector.Directory, operation string) error {
	if h == nil || h.agent == nil || !h.agent.runtimeAvailable() {
		return errors.New("agentsdk: connector runtime is unavailable")
	}
	wanted, exists := h.directories[directory.Name()]
	if !exists || directory.ContractID() != h.contractID || !sameDirectoryContract(wanted, directory.Descriptor()) {
		return errors.New("agentsdk: connector directory is not declared by the need")
	}
	switch operation {
	case "list":
		if !wanted.List {
			return errors.New("agentsdk: connector directory does not declare list")
		}
	case "stat", "read", "export":
		if !wanted.Read {
			return errors.New("agentsdk: connector directory does not declare read")
		}
	default:
		if !wanted.Write {
			return errors.New("agentsdk: connector directory does not declare write")
		}
	}
	return nil
}

type ConnectorOrchestrationHandle[Out any] struct {
	agent *Agent
	need  string
	id    string
}

func StartConnectorOrchestration[In, Out any](ctx context.Context, handle *ConnectorHandle, command connector.Command[In, Out], request protocol.OrchestrationRequest, input In) (*ConnectorOrchestrationHandle[Out], error) {
	if handle == nil || handle.agent == nil || !handle.agent.runtimeAvailable() {
		return nil, errors.New("agentsdk: connector runtime is unavailable")
	}
	if !handle.agent.connectors[handle.slug].Multiple {
		return nil, errors.New("agentsdk: connector need is not multi-target")
	}
	requestID, err := uuid.Parse(request.RequestID)
	if err != nil || requestID == uuid.Nil {
		return nil, errors.New("agentsdk: orchestration requires a caller-generated request UUID")
	}
	if command.Mode() != protocol.CommandModeJob {
		return nil, errors.New("agentsdk: orchestration requires a job-mode command")
	}
	descriptor, exists := handle.commands[command.Name()]
	if !exists || command.ContractID() != handle.contractID || !sameCommandContract(descriptor, command.Descriptor()) {
		return nil, errors.New("agentsdk: orchestration command is not declared")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(raw) > protocol.MaxJobPayloadBytes {
		return nil, fmt.Errorf("agentsdk: connector orchestration input exceeds %d bytes", protocol.MaxJobPayloadBytes)
	}
	request.Command = protocol.CommandCallRequest{Revision: descriptor.Revision, Mode: descriptor.Mode, InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: raw, Deadline: request.Deadline}
	var result wire.ConnectorOrchestrationInfo
	err = handle.agent.client.doJSON(ctx, http.MethodPost, "/api/agent/connectors/"+url.PathEscape(handle.slug)+"/orchestrations", wire.ConnectorOrchestrationRequest{CommandName: command.Name(), Request: request}, &result)
	if err != nil {
		return nil, err
	}
	if result.ID == "" {
		return nil, errors.New("agentsdk: Airlock returned an empty connector orchestration ID")
	}
	return &ConnectorOrchestrationHandle[Out]{agent: handle.agent, need: handle.slug, id: result.ID}, nil
}

func (h *ConnectorOrchestrationHandle[Out]) ID() string {
	if h == nil {
		return ""
	}
	return h.id
}

func (h *ConnectorOrchestrationHandle[Out]) Get(ctx context.Context) (wire.ConnectorOrchestrationInfo, error) {
	var result wire.ConnectorOrchestrationInfo
	if h == nil || h.agent == nil {
		return result, errors.New("agentsdk: connector orchestration handle is required")
	}
	err := h.agent.client.doJSON(ctx, http.MethodGet, "/api/agent/connectors/"+url.PathEscape(h.need)+"/orchestrations/"+url.PathEscape(h.id), nil, &result)
	return result, err
}

func (h *ConnectorOrchestrationHandle[Out]) Wait(ctx context.Context) (wire.ConnectorOrchestrationInfo, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := h.Get(ctx)
		if err != nil {
			return info, err
		}
		switch info.Status {
		case "success":
			return info, nil
		case "error", "canceled", "timeout":
			return info, connectorOrchestrationFailure(info)
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-ticker.C:
		}
	}
}

func connectorOrchestrationFailure(info wire.ConnectorOrchestrationInfo) error {
	if info.Status != "error" && info.Status != "canceled" && info.Status != "timeout" {
		return nil
	}
	details := make([]string, 0)
	for _, target := range info.Targets {
		if target.Error != "" {
			details = append(details, target.JobID+": "+target.Error)
		}
	}
	if len(details) == 0 {
		return fmt.Errorf("agentsdk: connector orchestration %s %s", info.ID, info.Status)
	}
	return fmt.Errorf("agentsdk: connector orchestration %s %s: %s", info.ID, info.Status, strings.Join(details, "; "))
}

func (h *ConnectorOrchestrationHandle[Out]) Cancel(ctx context.Context) error {
	if h == nil || h.agent == nil {
		return errors.New("agentsdk: connector orchestration handle is required")
	}
	return h.agent.client.doJSON(ctx, http.MethodDelete, "/api/agent/connectors/"+url.PathEscape(h.need)+"/orchestrations/"+url.PathEscape(h.id), nil, nil)
}

func cloneConnectorRequirement(value protocol.Requirement) protocol.Requirement {
	result := protocol.Requirement{ContractID: value.ContractID, Commands: make([]protocol.CommandDescriptor, len(value.Commands)), Directories: append([]protocol.DirectoryDescriptor(nil), value.Directories...)}
	for i, command := range value.Commands {
		result.Commands[i] = command
		result.Commands[i].InputSchema = append(json.RawMessage(nil), command.InputSchema...)
		result.Commands[i].OutputSchema = append(json.RawMessage(nil), command.OutputSchema...)
	}
	return result
}

func sameCommandContract(a, b protocol.CommandDescriptor) bool {
	return a.Name == b.Name && a.Revision == b.Revision && a.Mode == b.Mode && a.InputSchemaHash == b.InputSchemaHash && a.OutputSchemaHash == b.OutputSchemaHash
}

func sameDirectoryContract(a, b protocol.DirectoryDescriptor) bool {
	return a.Name == b.Name && a.Revision == b.Revision && a.Read == b.Read && a.Write == b.Write && a.List == b.List
}

func strictDecodeConnector(body []byte, result any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agentsdk: connector response has trailing JSON")
	}
	return nil
}
