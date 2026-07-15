package scaffold

import (
	"path"
	"strings"
)

// GeneratedArtifactIgnorePatterns is the scaffold-owned policy for disposable
// build outputs. Callers receive a copy so the policy cannot be mutated.
func GeneratedArtifactIgnorePatterns() []string {
	return []string{
		"*_templ.go",
		"views/static/app.css",
		"internal/db/*",
		"!internal/db/doc.go",
		"/agent",
		"/agent.exe",
	}
}

// IsGeneratedArtifact reports whether a root-relative agent repo path is a
// disposable output covered by GeneratedArtifactIgnorePatterns.
func IsGeneratedArtifact(name string) bool {
	name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, "\\", "/")), "./")
	if strings.HasSuffix(path.Base(name), "_templ.go") {
		return true
	}
	if name == "views/static/app.css" || name == "agent" || name == "agent.exe" {
		return true
	}
	return path.Dir(name) == "internal/db" && name != "internal/db/doc.go"
}
