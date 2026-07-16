package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const agentBindingPath = ".airlock/local/agent.toml"
const defaultRemoteName = "default"

type agentBinding struct {
	DefaultRemote string
	Remotes       map[string]agentRemoteBinding
}

type agentRemoteBinding struct {
	AirlockURL  string
	AgentID     string
	Slug        string
	SourceState string
}

func loadAgentBinding(dir string) (agentBinding, bool, error) {
	path := filepath.Join(dir, agentBindingPath)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agentBinding{}, false, nil
		}
		return agentBinding{}, false, err
	}
	defer f.Close()

	b := agentBinding{Remotes: map[string]agentRemoteBinding{}}
	currentRemote := ""
	seenSections := map[string]bool{}
	seenKeys := map[string]bool{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return agentBinding{}, false, fmt.Errorf("%s: invalid section %q", path, line)
			}
			name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if !strings.HasPrefix(name, "remotes.") {
				return agentBinding{}, false, fmt.Errorf("%s: unknown section %q", path, name)
			}
			currentRemote = strings.TrimPrefix(name, "remotes.")
			if !validRemoteName(currentRemote) {
				return agentBinding{}, false, fmt.Errorf("%s: invalid remote name %q", path, currentRemote)
			}
			if seenSections[currentRemote] {
				return agentBinding{}, false, fmt.Errorf("%s: duplicate remote section %q", path, currentRemote)
			}
			seenSections[currentRemote] = true
			b.Remotes[currentRemote] = agentRemoteBinding{}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return agentBinding{}, false, fmt.Errorf("%s: invalid line %q", path, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return agentBinding{}, false, fmt.Errorf("%s: invalid value for %s: %w", path, key, err)
		}
		scopedKey := currentRemote + "\x00" + key
		if seenKeys[scopedKey] {
			return agentBinding{}, false, fmt.Errorf("%s: duplicate key %q", path, key)
		}
		seenKeys[scopedKey] = true
		if currentRemote == "" {
			switch key {
			case "default_remote":
				b.DefaultRemote = unquoted
			default:
				return agentBinding{}, false, fmt.Errorf("%s: unknown top-level key %q", path, key)
			}
			continue
		}
		remote := b.Remotes[currentRemote]
		switch key {
		case "url":
			remote.AirlockURL = normalizeBaseURL(unquoted)
		case "agent_id":
			remote.AgentID = unquoted
		case "slug":
			remote.Slug = unquoted
		case "source_state":
			remote.SourceState = unquoted
		default:
			return agentBinding{}, false, fmt.Errorf("%s: unknown key %q", path, key)
		}
		b.Remotes[currentRemote] = remote
	}
	if err := s.Err(); err != nil {
		return agentBinding{}, false, err
	}
	if b.DefaultRemote == "" && len(b.Remotes) > 0 {
		return agentBinding{}, false, fmt.Errorf("%s: default_remote is required", path)
	}
	if b.DefaultRemote != "" {
		if !validRemoteName(b.DefaultRemote) {
			return agentBinding{}, false, fmt.Errorf("%s: invalid default remote name %q", path, b.DefaultRemote)
		}
		if _, ok := b.Remotes[b.DefaultRemote]; !ok {
			return agentBinding{}, false, fmt.Errorf("%s: default remote %q is not defined", path, b.DefaultRemote)
		}
	}
	return b, true, nil
}

func writeAgentBinding(dir string, b agentBinding) error {
	if b.DefaultRemote == "" {
		return fmt.Errorf("%s: default_remote is required", agentBindingPath)
	}
	if !validRemoteName(b.DefaultRemote) {
		return fmt.Errorf("%s: invalid default remote name %q", agentBindingPath, b.DefaultRemote)
	}
	if _, ok := b.Remotes[b.DefaultRemote]; !ok {
		return fmt.Errorf("%s: default remote %q is not defined", agentBindingPath, b.DefaultRemote)
	}
	for name := range b.Remotes {
		if !validRemoteName(name) {
			return fmt.Errorf("%s: invalid remote name %q", agentBindingPath, name)
		}
	}
	var content strings.Builder
	fmt.Fprintf(&content, "default_remote = %s\n", strconv.Quote(b.DefaultRemote))
	var names []string
	for name := range b.Remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		remote := b.Remotes[name]
		fmt.Fprintf(&content, "\n[remotes.%s]\n", name)
		if remote.AirlockURL != "" {
			fmt.Fprintf(&content, "url = %s\n", strconv.Quote(normalizeBaseURL(remote.AirlockURL)))
		}
		if remote.AgentID != "" {
			fmt.Fprintf(&content, "agent_id = %s\n", strconv.Quote(remote.AgentID))
		}
		if remote.Slug != "" {
			fmt.Fprintf(&content, "slug = %s\n", strconv.Quote(remote.Slug))
		}
		if remote.SourceState != "" {
			fmt.Fprintf(&content, "source_state = %s\n", strconv.Quote(remote.SourceState))
		}
	}

	path := filepath.Join(dir, agentBindingPath)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(parent, ".agent.toml-*")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.WriteString(content.String()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	closed = true
	return os.Rename(tmpPath, path)
}

func (b agentBinding) remote(name string) (agentRemoteBinding, bool) {
	if name == "" {
		name = b.DefaultRemote
	}
	if name == "" {
		return agentRemoteBinding{}, false
	}
	remote, ok := b.Remotes[name]
	return remote, ok
}

func (b *agentBinding) putRemote(name string, remote agentRemoteBinding) {
	if name == "" {
		name = b.DefaultRemote
	}
	if name == "" {
		name = defaultRemoteName
	}
	if b.Remotes == nil {
		b.Remotes = map[string]agentRemoteBinding{}
	}
	if b.DefaultRemote == "" {
		b.DefaultRemote = name
	}
	b.Remotes[name] = remote
}

func (b *agentBinding) setDefaultRemote(name string) error {
	if !validRemoteName(name) {
		return fmt.Errorf("invalid remote %q: use letters, digits, dashes, and underscores", name)
	}
	if _, ok := b.Remotes[name]; !ok {
		return fmt.Errorf("remote %q is not defined in %s", name, agentBindingPath)
	}
	b.DefaultRemote = name
	return nil
}

func validRemoteName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
