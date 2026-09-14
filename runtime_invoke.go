package agentsdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

const maxRuntimeInvokeBytes = 4 << 20

func (a *Agent) handleRuntimeInvoke(w http.ResponseWriter, r *http.Request) {
	// Handler authenticates host delivery. Invocation attribution comes only
	// from the scoped protocol body, not X-Caller-Access or X-Run-ID headers.
	var req wire.RuntimeInvokeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRuntimeInvokeBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid invocation", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid invocation", http.StatusBadRequest)
		return
	}
	if err := wire.CheckAppRuntimeProtocol(req.RuntimeProtocol); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if req.Context.AgentID != a.agentID {
		http.Error(w, "agent scope mismatch", http.StatusForbidden)
		return
	}
	if err := validateRuntimeContext(req.Context); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.CapabilityID == "" || req.ToolCallID == "" || !json.Valid(req.Input) {
		http.Error(w, "capabilityId, toolCallId and input are required", http.StatusBadRequest)
		return
	}
	manifest := a.Manifest()
	var scoped *registeredAgent
	if scope := req.Context.Definition; scope != nil {
		scoped = a.agentDefinitions[scope.Slug]
		if scoped == nil || scoped.definition.ContractHash != scope.ContractHash {
			http.Error(w, "agent definition contract mismatch", http.StatusConflict)
			return
		}
	}
	var catalog []capability.Definition
	var err error
	if scoped != nil {
		catalog, err = capability.DefinitionCatalog(manifest, *req.Context.Definition, capability.Discovery{})
	} else {
		catalog, err = capability.Catalog(manifest, capability.Discovery{})
	}
	if err != nil {
		http.Error(w, "invalid agent capability catalogue", http.StatusInternalServerError)
		return
	}
	var selected *capability.Definition
	for i := range catalog {
		if catalog[i].Path.ID() == req.CapabilityID {
			selected = &catalog[i]
			break
		}
	}
	if selected == nil || selected.Target != capability.App {
		http.Error(w, "unknown app capability", http.StatusNotFound)
		return
	}
	if !accessSatisfies(Access(req.Context.Caller.Access), Access(selected.Access)) {
		http.Error(w, "capability requires higher access", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultTimeout)
	defer cancel()
	active := a.borrowRuntimeRun(ctx, req.Context)
	defer active.cleanupScratch()
	var executable tool.Tool
	if selected.Path.Kind() == capability.Tool {
		if scoped != nil {
			// A scoped callback never resolves through the global tool registry.
			executable = scoped.tools[selected.Path.CanonicalOperation()]
		} else {
			rt, ok := a.tools[selected.Path.CanonicalOperation()]
			if !ok || !accessSatisfies(active.callerAccess, rt.access) {
				http.Error(w, "tool registration unavailable", http.StatusForbidden)
				return
			}
			executable = rt.Tool
		}
	} else {
		executable = runtimeLocalTools(a, active)[selected.Path.CanonicalOperation()]
	}
	if executable.Execute == nil {
		http.Error(w, "missing app capability executor", http.StatusInternalServerError)
		return
	}
	response := a.executeRuntimeTool(active, executable, req)
	secrets := []string{a.token, req.Context.InvocationToken}
	if req.Context.Job != nil {
		secrets = append(secrets, req.Context.Job.LeaseToken)
	}
	payload, err := a.marshalRuntimeResponse(response, secrets)
	if err != nil {
		http.Error(w, "invalid tool result", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(payload)
}

func validateRuntimeContext(c wire.RuntimeContext) error {
	if token, err := hex.DecodeString(c.InvocationToken); err != nil || len(token) != 32 || hex.EncodeToString(token) != c.InvocationToken {
		return errors.New("invalid invocation token")
	}
	if c.AgentID == "" || c.RunID == "" {
		return errors.New("agentId and runId are required")
	}
	for _, id := range []string{c.RunID, c.ConversationID, c.BridgeID} {
		if id == "" {
			continue
		}
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
			return errors.New("invalid runtime context ID")
		}
	}
	if err := c.Caller.Validate(); err != nil {
		return err
	}
	if d := c.Definition; d != nil {
		if !localIdentifierPattern.MatchString(d.Slug) || len(d.Slug) > 58 {
			return errors.New("invalid agent definition slug")
		}
		if hash, err := hex.DecodeString(d.ContractHash); err != nil || len(hash) != 32 || hex.EncodeToString(hash) != d.ContractHash {
			return errors.New("invalid agent definition contract hash")
		}
		if c.Caller.Kind != "application" || c.Caller.Initiator != nil || c.Caller.Access != wire.AccessAdmin || c.Job != nil {
			return errors.New("agent definition requires an application-owned caller without a job fence")
		}
	}
	if c.Job != nil {
		if err := validateJobID(c.Job.ID); err != nil {
			return errors.New("invalid job ID")
		}
		if c.Job.Attempt < 1 {
			return errors.New("invalid job attempt")
		}
		if err := validateJobID(c.Job.LeaseToken); err != nil {
			return errors.New("invalid job lease token")
		}
	}
	return nil
}

// runtimeLocalTools contains only app-owned operations. Platform services do
// not take an unnecessary app hop. Tests compare this entire map to Fixed.
func runtimeLocalTools(a *Agent, r *run) map[string]tool.Tool {
	return map[string]tool.Tool{
		"file_read":             wrapFileRead(a, r),
		"file_read_bytes":       wrapFileReadBytes(a, r),
		"file_read_range_bytes": wrapFileReadRangeBytes(a, r),
		"file_grep":             wrapFileGrep(a, r),
		"file_head":             wrapFileHead(a, r),
		"file_tail":             wrapFileTail(a, r),
		"file_lines":            wrapFileLines(a, r),
		"file_stat":             wrapFileStat(a, r),
		"file_exists":           wrapFileExists(a, r),
		"file_encode":           wrapFileEncode(a, r),
		"file_decode":           wrapFileDecode(a, r),
		"file_decode_text":      wrapFileDecodeText(a, r),
		"file_edit_lines":       wrapFileEditLines(a, r),
		"file_sed":              wrapFileSed(a, r),
		"file_write":            wrapFileWrite(a, r),
		"file_delete":           wrapFileDelete(a, r),
		"file_list":             wrapFileList(a, r),
		"query_db":              wrapQueryDB(a, r),
	}
}

func (a *Agent) executeRuntimeTool(active *run, executable tool.Tool, req wire.RuntimeInvokeRequest) (response wire.RuntimeInvokeResponse) {
	response.RuntimeProtocol = wire.AppRuntimeProtocol
	defer func() {
		if recover() != nil {
			response.Error = "tool panicked"
		}
		active.mu.Lock()
		response.Logs = append([]wire.LogEntry(nil), active.logs...)
		response.Actions = append([]wire.Action(nil), active.actions...)
		active.mu.Unlock()
	}()
	ctx := active.checkedCtx()
	result, err := executable.Execute(ctx, req.Input, tool.CallOptions{ToolCallID: req.ToolCallID, AbortSignal: ctx})
	response.Output, response.Title, response.Metadata = result.Output, result.Title, result.Metadata
	if err != nil {
		response.Error = err.Error()
	}
	for _, attachment := range result.Attachments {
		ref, err := a.runtimeAttachment(ctx, attachment)
		if err != nil {
			response.Warnings = append(response.Warnings, "attachment unavailable: "+err.Error())
			continue
		}
		response.Attachments = append(response.Attachments, ref)
	}
	return response
}

func (a *Agent) runtimeAttachment(ctx context.Context, attachment tool.Attachment) (wire.RuntimeAttachment, error) {
	ref := wire.RuntimeAttachment{MimeType: attachment.MimeType, Filename: attachment.Filename}
	if strings.HasPrefix(attachment.Data, s3RefSentinel) {
		path, err := a.resolveFilePath(ctx, strings.TrimPrefix(attachment.Data, s3RefSentinel), FileOperationRead)
		if err != nil {
			return ref, err
		}
		ref.Path = path
		return ref, nil
	}
	// Author tools may return inline goai attachments. Persist once, then send
	// only the object reference across the broker boundary, never NDJSON bytes.
	data, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil {
		return ref, errors.New("invalid base64 attachment")
	}
	path := reservedTmpPath + "/attachments/" + uuid.NewString()
	info, err := a.WriteFile(ctx, path, bytes.NewReader(data), attachment.MimeType)
	if err != nil {
		return ref, fmt.Errorf("store attachment: %w", err)
	}
	ref.Path, ref.MimeType = string(info.Path), info.ContentType
	return ref, nil
}

// Redact decoded JSON strings, including nested JSON tool output, rather than
// replacing bytes in serialized JSON (which misses escaped secrets and can
// corrupt the protocol). UseNumber preserves numeric metadata exactly.
func (a *Agent) marshalRuntimeResponse(response wire.RuntimeInvokeResponse, secrets []string) ([]byte, error) {
	a.sensitiveM.RLock()
	values := append([]string(nil), secrets...)
	for value := range a.sensitiveSet {
		values = append(values, value)
	}
	a.sensitiveM.RUnlock()
	// Longest-first prevents a shorter credential prefix from exposing the
	// remainder of an overlapping secret. Snapshot once for the whole response.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	changed := false
	redactString := func(value string) string {
		original := value
		for _, secret := range values {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[REDACTED]")
			}
		}
		changed = changed || value != original
		return value
	}
	var redact func(any) any
	redact = func(value any) any {
		switch v := value.(type) {
		case string:
			return redactString(v)
		case []any:
			for i := range v {
				v[i] = redact(v[i])
			}
		case map[string]any:
			out := make(map[string]any, len(v))
			for key, value := range v {
				out[redactString(key)] = redact(value)
			}
			return out
		}
		return value
	}
	decode := func(data []byte) (any, error) {
		var value any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		err := dec.Decode(&value)
		return value, err
	}
	if json.Valid([]byte(response.Output)) {
		value, err := decode([]byte(response.Output))
		if err != nil {
			return nil, err
		}
		output, err := json.Marshal(redact(value))
		if err != nil {
			return nil, err
		}
		if changed {
			response.Output = string(output)
		}
	} else {
		response.Output = redactString(response.Output)
	}
	// Only dynamic JSON objects have redacted keys. Protocol field names must
	// remain intact even when an author registers a common word as sensitive.
	redactDynamic := func(value any) (any, error) {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		value, err = decode(data)
		if err != nil {
			return nil, err
		}
		return redact(value), nil
	}
	response.Title, response.Error = redactString(response.Title), redactString(response.Error)
	if response.Metadata != nil {
		value, err := redactDynamic(response.Metadata)
		if err != nil {
			return nil, err
		}
		response.Metadata = value.(map[string]any)
	}
	for i := range response.Warnings {
		response.Warnings[i] = redactString(response.Warnings[i])
	}
	for i := range response.Logs {
		response.Logs[i].Message = redactString(response.Logs[i].Message)
	}
	for i := range response.Attachments {
		ref := &response.Attachments[i]
		ref.Path, ref.MimeType, ref.Filename = redactString(ref.Path), redactString(ref.MimeType), redactString(ref.Filename)
	}
	for i := range response.Actions {
		action := &response.Actions[i]
		action.Type, action.Error = redactString(action.Type), redactString(action.Error)
		var err error
		action.Request, err = redactDynamic(action.Request)
		if err != nil {
			return nil, err
		}
		action.Response, err = redactDynamic(action.Response)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(response)
}
