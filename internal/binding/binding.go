// Package binding maps canonical capabilities to stable presentation names.
package binding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

// MaxDirectNameLength is the provider-facing tool-name limit.
const MaxDirectNameLength = 64

// Kind identifies a capability namespace.
type Kind byte

const (
	Air Kind = iota + 1
	Tool
	Connection
	Topic
	MCP
	Agent
)

// Path carries canonical transport identity and presentation aliases.
type Path struct {
	kind               Kind
	canonicalNamespace string
	canonicalOperation string
	namespaceAlias     string
	operationAlias     string
	noJS               bool
}

// Local creates a path whose canonical and presentation names are identical.
func Local(kind Kind, namespace, operation string) Path {
	return Path{
		kind:               kind,
		canonicalNamespace: namespace,
		canonicalOperation: operation,
		namespaceAlias:     namespace,
		operationAlias:     operation,
	}
}

// AgentPrompt returns the direct tool used for open-ended agent delegation.
func AgentPrompt() Path {
	return Path{kind: Agent, canonicalOperation: "prompt", operationAlias: "prompt", noJS: true}
}

// External creates collision-safe paths for arbitrary external operation names.
func External(kind Kind, canonicalNamespace, namespaceAlias string, operations []string) (map[string]Path, error) {
	if kind != MCP && kind != Agent {
		return nil, errors.New("external bindings require MCP or Agent kind")
	}
	bases := make(map[string][]string, len(operations))
	seen := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		if _, ok := seen[operation]; ok {
			return nil, errors.New("duplicate canonical operation: " + operation)
		}
		seen[operation] = struct{}{}
		base := normalize(operation)
		bases[base] = append(bases[base], operation)
	}

	aliases := make(map[string]string, len(operations))
	for base, names := range bases {
		sort.Strings(names)
		for _, name := range names {
			alias := base
			if len(names) > 1 {
				alias += "_h" + pathHash(kind, canonicalNamespace, name)
			}
			aliases[name] = alias
		}
	}

	paths := make(map[string]Path, len(operations))
	for operation, alias := range aliases {
		paths[operation] = Path{
			kind:               kind,
			canonicalNamespace: canonicalNamespace,
			canonicalOperation: operation,
			namespaceAlias:     namespaceAlias,
			operationAlias:     alias,
		}
	}
	return paths, nil
}

// SiblingNamespace maps a canonical kebab-case agent slug to a JS-safe alias.
func SiblingNamespace(slug string) string {
	alias := strings.ReplaceAll(slug, "-", "_")
	if alias != "" && alias[0] >= '0' && alias[0] <= '9' {
		alias = "_" + alias
	}
	return alias
}

// JSParts returns the object-property path used by run_js.
func (p Path) JSParts() []string {
	if p.noJS {
		return nil
	}
	switch p.kind {
	case Air:
		return []string{"air", builtinJSName(p.operationAlias)}
	case Tool:
		return []string{"tools", p.operationAlias}
	case Connection:
		return []string{"conn", p.namespaceAlias, builtinJSName(p.operationAlias)}
	case Topic:
		return []string{"topic", p.namespaceAlias, builtinJSName(p.operationAlias)}
	case MCP:
		return []string{"mcp", p.namespaceAlias, p.operationAlias}
	case Agent:
		return []string{"agent", p.namespaceAlias, p.operationAlias}
	default:
		panic("binding: unknown kind")
	}
}

// JS returns the dotted run_js presentation path.
func (p Path) JS() string {
	return strings.Join(p.JSParts(), ".")
}

// Direct returns the provider-facing direct tool name.
func (p Path) Direct() string {
	name := kindName(p.kind) + "__"
	if p.namespaceAlias != "" {
		name += p.namespaceAlias + "__"
	}
	name += p.operationAlias
	if len(name) <= MaxDirectNameLength {
		return name
	}
	suffix := "_h" + pathHash(p.kind, p.canonicalNamespace, p.canonicalOperation)
	return name[:MaxDirectNameLength-len(suffix)] + suffix
}

// CanonicalNamespace returns the namespace used by transport dispatch.
func (p Path) CanonicalNamespace() string { return p.canonicalNamespace }

// CanonicalOperation returns the operation used by transport dispatch.
func (p Path) CanonicalOperation() string { return p.canonicalOperation }

func normalize(name string) string {
	var b strings.Builder
	separator := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
			separator = false
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			separator = false
		default:
			if b.Len() > 0 {
				separator = true
			}
		}
		if separator && i+1 < len(name) {
			next := name[i+1]
			if next >= 'A' && next <= 'Z' || next >= 'a' && next <= 'z' || next >= '0' && next <= '9' {
				b.WriteByte('_')
				separator = false
			}
		}
	}
	alias := strings.Trim(b.String(), "_")
	if alias == "" {
		alias = "tool"
	}
	if alias[0] >= '0' && alias[0] <= '9' {
		alias = "tool_" + alias
	}
	return alias
}

func builtinJSName(name string) string {
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		switch parts[i] {
		case "db", "http", "id", "json", "llm", "mcp", "pdf", "url":
			parts[i] = strings.ToUpper(parts[i])
		default:
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
	}
	return strings.Join(parts, "")
}

func kindName(kind Kind) string {
	switch kind {
	case Air:
		return "air"
	case Tool:
		return "tool"
	case Connection:
		return "conn"
	case Topic:
		return "topic"
	case MCP:
		return "mcp"
	case Agent:
		return "agent"
	default:
		panic("binding: unknown kind")
	}
}

func pathHash(kind Kind, namespace, operation string) string {
	data := make([]byte, 1+4+len(namespace)+4+len(operation))
	data[0] = byte(kind)
	offset := 1
	binary.BigEndian.PutUint32(data[offset:], uint32(len(namespace)))
	offset += 4
	copy(data[offset:], namespace)
	offset += len(namespace)
	binary.BigEndian.PutUint32(data[offset:], uint32(len(operation)))
	offset += 4
	copy(data[offset:], operation)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}
