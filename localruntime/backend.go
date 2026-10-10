package localruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

// AppBackend dispatches through the actual authenticated v3 app listener.
// The admitted scope and catalog are host-owned, not supplied by callback input.
type AppBackend struct {
	URL      string
	Token    string
	Client   *http.Client
	Scope    wire.RuntimeContext
	Catalog  []capability.Definition
	Platform chatruntime.Backend
}

func (b *AppBackend) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	var selected *capability.Definition
	for i := range b.Catalog {
		if b.Catalog[i].Path.ID() == in.CapabilityID {
			selected = &b.Catalog[i]
			break
		}
	}
	if selected == nil {
		return tool.Result{}, errors.New("localruntime: unknown capability")
	}
	if selected.Target == capability.Platform {
		if b.Platform == nil {
			return tool.Result{}, fmt.Errorf("localruntime: platform capability %q requires an adapter", in.CapabilityID)
		}
		return b.Platform.Invoke(ctx, in)
	}
	if selected.Target != capability.App {
		return tool.Result{}, errors.New("localruntime: executor intrinsic cannot be dispatched")
	}
	id := in.CapabilityID
	if selected.Path.Kind() == capability.AppTool {
		id = capability.Local(capability.Tool, "", selected.Path.CanonicalOperation()).ID()
	}
	if b.Client == nil || b.Token == "" || b.URL == "" {
		return tool.Result{}, errors.New("localruntime: app URL, token and HTTP client are required")
	}
	raw, err := json.Marshal(wire.RuntimeInvokeRequest{RuntimeProtocol: wire.AppRuntimeProtocol, Context: b.Scope, CapabilityID: id, ToolCallID: in.ToolCallID, Input: in.Input})
	if err != nil {
		return tool.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", b.URL+wire.RuntimeInvokePath, bytes.NewReader(raw))
	if err != nil {
		return tool.Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := b.Client.Do(req)
	if err != nil {
		return tool.Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return tool.Result{}, fmt.Errorf("localruntime: app invocation HTTP %d", response.StatusCode)
	}
	var out wire.RuntimeInvokeResponse
	if err := strictJSON(response.Body, &out); err != nil {
		return tool.Result{}, err
	}
	if err := wire.CheckAppRuntimeProtocol(out.RuntimeProtocol); err != nil {
		return tool.Result{}, err
	}
	result := tool.Result{Output: out.Output, Title: out.Title, Metadata: out.Metadata}
	for _, ref := range out.Attachments {
		result.Attachments = append(result.Attachments, tool.Attachment{Data: "s3ref:" + ref.Path, MimeType: ref.MimeType, Filename: ref.Filename})
	}
	if out.Error != "" {
		return result, errors.New(out.Error)
	}
	return result, nil
}
