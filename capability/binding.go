// Package capability defines the manifest-based capability catalogue shared by
// Airlock's broker and agent runtimes. It does not import the root SDK package.
package capability

import "github.com/airlockrun/agentsdk/internal/binding"

// Path gives one canonical identity both JS and direct-tool presentations.
type Path = binding.Path
type Kind = binding.Kind

const (
	Air                 = binding.Air
	Tool                = binding.Tool
	Connection          = binding.Connection
	Topic               = binding.Topic
	MCP                 = binding.MCP
	Host                = binding.Host
	AppTool             = binding.AppTool
	AppFile             = binding.AppFile
	AppDatabase         = binding.AppDatabase
	ResourceTool        = binding.ResourceTool
	ResourceConnection  = binding.ResourceConnection
	MaxDirectNameLength = binding.MaxDirectNameLength
)

func Local(kind Kind, namespace, operation string) Path {
	return binding.Local(kind, namespace, operation)
}

func External(kind Kind, namespace, alias string, operations []string) (map[string]Path, error) {
	return binding.External(kind, namespace, alias, operations)
}

func Qualified(kind Kind, target, alias, operation string) Path {
	return binding.Qualified(kind, target, alias, operation)
}

func AppAlias(slug string) string { return binding.AppAlias(slug) }
