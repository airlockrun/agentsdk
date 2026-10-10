package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
)

// The SDK app handler gates stat/read/write against its real directory and run
// policies. The only platform operation supplied by this backend is attachment.
type storageBackend struct {
	app      *chatBackend
	fallback chatruntime.Backend
}

func (b *storageBackend) Invoke(ctx context.Context, in chatruntime.Invocation) (tool.Result, error) {
	if in.CapabilityID != "air//attach_to_context" {
		if b.fallback != nil {
			return b.fallback.Invoke(ctx, in)
		}
		return tool.Result{}, fmt.Errorf("agenttest: unsupported service %q; supply Backend", in.CapabilityID)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(in.Input, &args); err != nil || args.Path == "" {
		return tool.Result{}, errors.New("agenttest: attachment path required")
	}
	// Reuse authenticated SDK path resolution rather than duplicating directory
	// ACLs and user/conversation/run scope insertion in test support.
	result, err := b.app.Invoke(ctx, chatruntime.Invocation{CapabilityID: "air//file_stat", ToolCallID: in.ToolCallID, Input: in.Input})
	if err != nil {
		return tool.Result{}, err
	}
	var info wire.FileInfo
	if err := json.Unmarshal([]byte(result.Output), &info); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Output: "Attached " + info.Path, Attachments: []tool.Attachment{{Data: "s3ref:" + info.Path, MimeType: info.ContentType, Filename: info.Filename}}}, nil
}
