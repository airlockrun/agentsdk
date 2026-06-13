package agentsdk

import (
	"reflect"

	"github.com/airlockrun/goai/schema"
)

// FilePath is a string-typed alias for storage paths owned by this agent.
// Use it for tool input/output fields that name a file the tool reads,
// writes, returns, or consumes. Inside the same process (run_js, Go code)
// it behaves as a plain string.
//
// At MCP boundaries airlock rewrites the path so callees always see one
// readable in their own bucket: cross-bucket copy for A2A, base64
// materialization for external MCP clients. Authors don't need to think
// about this — declaring `FilePath` is the entire opt-in.
type FilePath string

// DirPath is a string-typed alias for storage directory paths. Use it
// when a tool argument or return value names a directory rather than a
// single file. Inside the same process it behaves as a plain string.
//
// Across MCP boundaries DirPath args/results are rejected with a clear
// JSON-RPC error — copying directory trees is unbounded and not
// supported. Authors wanting cross-boundary directory semantics should
// restructure as []FilePath so the caller picks exact files.
type DirPath string

func init() {
	schema.RegisterTypeOverride(reflect.TypeOf(FilePath("")), func(s *schema.Schema) {
		s.Format = "agent-file"
	})
	schema.RegisterTypeOverride(reflect.TypeOf(DirPath("")), func(s *schema.Schema) {
		s.Format = "agent-dir"
	})
}
