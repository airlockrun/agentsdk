package capability

import (
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
)

func fileInfoOutputSchema() *schema.Schema {
	output := schema.MustFromType(wire.FileInfo{})
	// time.Time marshals as RFC3339 text; reflection sees its private struct fields.
	output.Properties["lastModified"] = &schema.Schema{Type: "string", Format: "date-time"}
	return output
}

// These output contracts are shared by catalogue schemas and app executors.
type FileReadBytesOutput struct {
	Size   int    `json:"size"`
	Base64 string `json:"base64"`
}

type FileExistsOutput struct {
	Exists bool `json:"exists"`
}

type FileDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

// FileTransformInlineOutput includes content even when the result is empty.
type FileTransformInlineOutput struct {
	Inline  bool   `json:"inline"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
}

type FileTransformSavedOutput struct {
	Inline  bool   `json:"inline"`
	SavedTo string `json:"savedTo"`
	Preview string `json:"preview,omitempty"`
	Size    int64  `json:"size"`
}
