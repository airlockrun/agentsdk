package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
)

// LLM-facing envelope projections shared by the JS bindings (vm.go,
// vm_exec.go) and the direct-tool wrappers (directtools_builtins.go,
// directtools_dynamic.go). Each helper takes the Go-side result and
// returns the canonical map / struct both adapters serialize — JS via
// goja.Runtime.ToValue, direct via json.Marshal. The shape lives here
// so the two modes can never silently drift on field names, casing, or
// "inline vs spilled" key precedence.

// httpResponseToMap projects an httpResponse onto the JSON-shaped map the
// httpRequest tool returns. JSON-typed inline bodies are pre-parsed into
// `body`; spilled bodies surface savedTo + bodyPreview + note. The map
// renders identically whether the caller is the run_js binding (via
// vm.ToValue) or the direct-mode httpRequest tool (via json.Marshal).
func httpResponseToMap(resp *wire.HTTPResponse) map[string]any {
	out := map[string]any{
		"status":      resp.Status,
		"headers":     resp.Headers,
		"contentType": resp.ContentType,
		"size":        resp.Size,
	}
	if resp.SavedTo != "" {
		out["savedTo"] = resp.SavedTo
	}
	if resp.BodyPreview != "" {
		out["bodyPreview"] = resp.BodyPreview
	}
	if resp.Note != "" {
		out["note"] = resp.Note
	}
	if resp.Body != "" {
		if strings.Contains(resp.ContentType, "application/json") {
			var parsed any
			if err := json.Unmarshal([]byte(resp.Body), &parsed); err == nil {
				out["body"] = parsed
				return out
			}
		}
		out["body"] = resp.Body
	}
	return out
}

// connSpillResult is the Go-side projection of a connection request: the
// status / contentType / size envelope plus either an inline body (Inline,
// when !Spilled) or a savedTo path with a preview (when Spilled). Shared by
// the run_js conn_{slug}.request(JSON)? bindings and the direct-mode
// conn_{slug}_request(_json)? tools; each adapter projects this onto its
// own LLM-facing shape ({body} vs {data} for raw vs JSON mode).
type connSpillResult struct {
	Status      int
	ContentType string
	Size        int64
	Spilled     bool
	Inline      []byte // payload when !Spilled
	BodyPreview []byte // ~1 KiB head when Spilled
	SavedTo     string // storage path when Spilled
}

// connRequestExec runs handle.RequestStream and peekAndSpill, returning the
// shared connSpillResult. The bindings/tools that wrap it decide how to
// project the inline bytes (string body vs parsed JSON value) and what
// follow-up note to surface when the response spilled.
func connRequestExec(ctx context.Context, agent *Agent, handle *ConnectionHandle, slug string, opts RequestOpts) (*connSpillResult, error) {
	resp, err := handle.RequestStream(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	contentType := resp.Headers.Get("Content-Type")
	dst := fmt.Sprintf("tmp/conn-%s-%s.bin", slug, newCallID())
	in, savedTo, size, err := peekAndSpill(ctx, agent, resp.Body, dst, contentType)
	if err != nil {
		return nil, err
	}
	out := &connSpillResult{
		Status:      resp.StatusCode,
		ContentType: contentType,
		Size:        size,
	}
	if savedTo != "" {
		out.Spilled = true
		out.BodyPreview = in
		out.SavedTo = savedTo
		return out, nil
	}
	out.Inline = in
	return out, nil
}

// connSpilledNote is the LLM-facing note for a spilled conn response. mode
// chooses the read-back hint: "raw" → `air.fileRead(bodySavedTo)`, "json" →
// `JSON.parse(air.fileRead(bodySavedTo))`. Shared by both adapters so the two
// modes never drift on phrasing.
func connSpilledNote(size int64, savedTo, mode string) string {
	if mode == "json" {
		return fmt.Sprintf("Body (%d bytes) exceeded inline threshold; saved to %s. Read with: JSON.parse(air.fileRead(bodySavedTo)).", size, savedTo)
	}
	return fmt.Sprintf("Body (%d bytes) exceeded inline threshold; saved to %s. Use air.fileRead(bodySavedTo) to read the full body.", size, savedTo)
}
