package chatruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/tool"
)

const MaxScriptBytes = 256 << 10

type scriptFileInput struct {
	Path                string          `json:"path"`
	Args                json.RawMessage `json:"args"`
	Description         string          `json:"description"`
	ExpectedSHA256      string          `json:"expected_sha256,omitempty"`
	RequestConfirmation bool            `json:"request_confirmation,omitempty"`
}

func (r *runtime) scriptReader() *capability.Definition {
	for _, d := range r.input.Capabilities {
		if d.Path.ID() == "air//file_read" || d.Path.Kind() == capability.Host && d.Path.CanonicalOperation() == "files" {
			return &d
		}
	}
	return nil
}

func (r *runtime) scriptTool() tool.Tool {
	return tool.New("run_js_file").Description("Execute an authorized stored UTF-8 .js async function body. Structured args is a lexical JSON object, not code. Use logical storage paths, not host filesystem paths. The same sandbox, callback capabilities and budgets as run_js apply. Return results explicitly. Optional expected_sha256 pins the exact source bytes. No TypeScript transpilation or nested runner callback is available.").Schema(json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"args":{"type":"object","additionalProperties":true},"description":{"type":"string"},"expected_sha256":{"type":"string"},"request_confirmation":{"type":"boolean"}},"required":["path","args","description"],"additionalProperties":false}`)).Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		var in scriptFileInput
		if err := json.Unmarshal(input, &in); err != nil {
			return tool.Result{}, err
		}
		var args map[string]json.RawMessage
		if len(in.Args) > jsexec.MaxArgsBytes || json.Unmarshal(in.Args, &args) != nil || args == nil || strings.TrimSpace(in.Description) == "" {
			return tool.Result{}, errors.New("description and bounded object args are required")
		}
		if in.Path == "" || path.Clean(in.Path) != in.Path || strings.ContainsAny(in.Path, "\\\x00") || path.Ext(in.Path) != ".js" {
			return tool.Result{}, errors.New("canonical logical .js path required")
		}
		reader := r.scriptReader()
		if reader == nil {
			return tool.Result{}, errors.New("script file read capability is unavailable")
		}
		params := map[string]string{"path": in.Path}
		if reader.Path.Kind() == capability.Host {
			params["operation"] = "read"
		}
		payload, _ := json.Marshal(params)
		loaded, err := r.input.Backend.Invoke(ctx, Invocation{CapabilityID: reader.Path.ID(), ToolCallID: opts.ToolCallID, Input: payload})
		if err != nil {
			return tool.Result{}, err
		}
		code := loaded.Output
		resolved := in.Path
		var contentID string
		if reader.Path.Kind() == capability.Host {
			var out struct {
				Text string `json:"text"`
				File struct {
					Path      string `json:"path"`
					ContentID string `json:"contentId"`
				} `json:"file"`
			}
			if err := json.Unmarshal([]byte(code), &out); err != nil {
				return tool.Result{}, err
			}
			code, resolved, contentID = out.Text, out.File.Path, out.File.ContentID
		}
		if len(code) > MaxScriptBytes || !utf8.ValidString(code) || strings.TrimSpace(code) == "" {
			return tool.Result{}, errors.New("script must be nonempty bounded UTF-8 JavaScript")
		}
		hash := sha256.Sum256([]byte(code))
		digest := hex.EncodeToString(hash[:])
		if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
			return tool.Result{}, errors.New("script SHA-256 mismatch")
		}
		if resume := r.input.Resume; resume != nil && resume.ToolCallID == opts.ToolCallID {
			raw, err := json.Marshal(resume.Data)
			if err != nil {
				return tool.Result{}, err
			}
			var data struct {
				Metadata map[string]any `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &data); err != nil {
				return tool.Result{}, err
			}
			if data.Metadata["source_sha256"] != digest {
				return tool.Result{}, errors.New("script changed since confirmation")
			}
		}
		metadata := map[string]any{"source_path": resolved, "source_sha256": digest, "args": json.RawMessage(in.Args)}
		if contentID != "" {
			metadata["source_content_id"] = contentID
		}
		return r.executeCode(ctx, opts, "run_js_file", code, in.Description, in.RequestConfirmation, in.Args, metadata)
	}).Build()
}
