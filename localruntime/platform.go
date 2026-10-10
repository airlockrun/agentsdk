package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

type taskPlatform struct {
	host *Host
	app  *AppBackend
}

func (b *taskPlatform) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	var selected *capability.Definition
	for i := range b.app.Catalog {
		if b.app.Catalog[i].Path.ID() == in.CapabilityID {
			selected = &b.app.Catalog[i]
			break
		}
	}
	if selected == nil {
		return tool.Result{}, errors.New("localruntime: unknown platform capability")
	}
	switch selected.Path.Kind() {
	case capability.MCP:
		adapter, err := b.host.resourceAdapter(ctx, "mcp", selected.Path.CanonicalNamespace())
		if err != nil {
			return tool.Result{}, err
		}
		if adapter == nil {
			return tool.Result{}, errors.New("localruntime: MCP requires an explicit resource adapter")
		}
		if err := b.checkResource(ctx, "mcp", selected.Path.CanonicalNamespace()); err != nil {
			return tool.Result{}, err
		}
		out, err := adapter.MCP(ctx, b.app.Scope.Caller, selected.Path.CanonicalNamespace(), wire.MCPToolCallRequest{Tool: selected.Path.CanonicalOperation(), Arguments: in.Input})
		if err != nil {
			return tool.Result{}, err
		}
		raw, err := json.Marshal(out)
		return tool.Result{Output: string(raw)}, err
	case capability.Connection:
		adapter, err := b.host.resourceAdapter(ctx, "connection", selected.Path.CanonicalNamespace())
		if err != nil {
			return tool.Result{}, err
		}
		if adapter == nil {
			return tool.Result{}, errors.New("localruntime: connection requires an explicit resource adapter")
		}
		if err := b.checkResource(ctx, "connection", selected.Path.CanonicalNamespace()); err != nil {
			return tool.Result{}, err
		}
		var args capability.ConnectionRequestInput
		if err := json.Unmarshal(in.Input, &args); err != nil {
			return tool.Result{}, err
		}
		body := ""
		if text, ok := args.Body.(string); ok {
			body = text
		} else if args.Body != nil {
			raw, err := json.Marshal(args.Body)
			if err != nil {
				return tool.Result{}, err
			}
			body = string(raw)
		}
		out, err := adapter.Connection(ctx, b.app.Scope.Caller, selected.Path.CanonicalNamespace(), wire.ProxyRequest{Method: args.Method, Path: args.Path, Body: body, Headers: args.Headers})
		if err != nil {
			return tool.Result{}, err
		}
		defer out.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(out.Body, 20<<20+1))
		if err != nil {
			return tool.Result{}, err
		}
		if len(raw) > 20<<20 {
			return tool.Result{}, errors.New("localruntime: connection response exceeds 20 MiB")
		}
		if out.StatusCode < 200 || out.StatusCode >= 300 {
			return tool.Result{}, fmt.Errorf("connection returned HTTP %d", out.StatusCode)
		}
		return tool.Result{Output: string(raw)}, nil
	case capability.Air:
		if selected.Path.CanonicalOperation() == "attach_to_context" {
			out, err := b.app.Invoke(ctx, chatruntime.Invocation{CapabilityID: "air//file_stat", ToolCallID: in.ToolCallID, Input: in.Input})
			if err != nil {
				return tool.Result{}, err
			}
			var info wire.FileInfo
			if err := json.Unmarshal([]byte(out.Output), &info); err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: "Attached " + info.Path, Attachments: []tool.Attachment{{Data: "s3ref:" + info.Path, MimeType: info.ContentType, Filename: info.Filename}}}, nil
		}
	}
	if b.host.config.Platform != nil {
		return b.host.config.Platform.Invoke(ctx, in)
	}
	return tool.Result{}, fmt.Errorf("localruntime: platform capability %q requires an explicit adapter", in.CapabilityID)
}

func (b *taskPlatform) checkResource(ctx context.Context, kind, slug string) error {
	return b.host.config.State.update(ctx, func(d *stateData) error {
		if kind == "mcp" {
			for _, need := range d.Manifest.MCPServers {
				if need.Slug == slug {
					if need.BindingMode == wire.BindingPerUser {
						return errors.New("localruntime: application tasks cannot use per-user resources")
					}
					return nil
				}
			}
		}
		if kind == "connection" {
			for _, need := range d.Manifest.Connections {
				if need.Slug == slug {
					if need.BindingMode == wire.BindingPerUser {
						return errors.New("localruntime: application tasks cannot use per-user resources")
					}
					return nil
				}
			}
		}
		return errors.New("localruntime: resource need is not registered")
	})
}
