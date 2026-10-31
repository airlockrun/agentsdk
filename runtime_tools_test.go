package agentsdk

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/google/jsonschema-go/jsonschema"
)

func TestFileOperationOutputSchemasMatchRuntimeJSON(t *testing.T) {
	info := FileInfo{Path: "tmp/result", Filename: "result", ContentType: "text/plain", Size: 12, LastModified: time.Now()}
	inline := (&transformResult{inline: true}).output()
	saved := (&transformResult{savedTo: "tmp/result", preview: "preview", size: 12}).output()
	samples := map[string][]any{
		"file_read_bytes":       {fileReadBytesOutput{Size: 1, Base64: "YQ=="}},
		"file_read_range_bytes": {fileReadBytesOutput{}},
		"file_exists":           {fileExistsOutput{Exists: true}, fileExistsOutput{}},
		"file_delete":           {capability.FileDeleteOutput{Deleted: true}},
		"file_stat":             {info},
		"file_write":            {info},
		"file_list":             {[]FileInfo{info}, []FileInfo{}, []FileInfo(nil)},
		"file_encode":           {inline, saved},
		"file_decode":           {saved},
		"file_decode_text":      {inline, saved},
		"file_edit_lines":       {inline, saved},
		"file_sed":              {inline, saved},
	}
	for _, definition := range capability.Fixed() {
		op := definition.Path.CanonicalOperation()
		values, ok := samples[op]
		if !ok {
			continue
		}
		t.Run(op, func(t *testing.T) {
			var contract jsonschema.Schema
			if err := json.Unmarshal(definition.OutputSchema, &contract); err != nil {
				t.Fatal(err)
			}
			resolved, err := contract.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range values {
				result, err := jsonResult(value)
				if err != nil {
					t.Fatal(err)
				}
				var decoded any
				if err := json.Unmarshal([]byte(result.Output), &decoded); err != nil {
					t.Fatal(err)
				}
				if err := resolved.Validate(decoded); err != nil {
					t.Fatalf("runtime JSON %s does not match schema %s: %v", result.Output, definition.OutputSchema, err)
				}
			}
			if err := resolved.Validate(map[string]any{"unexpected": true}); err == nil {
				t.Fatal("unrelated output accepted")
			}
		})
	}
}
