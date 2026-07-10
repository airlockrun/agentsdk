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
			if _, ok := b.Remotes[currentRemote]; !ok {
				b.Remotes[currentRemote] = agentRemoteBinding{}
			}
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
	return b, true, nil
}

func writeAgentBinding(dir string, b agentBinding) error {
	path := filepath.Join(dir, agentBindingPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if b.DefaultRemote == "" {
		b.DefaultRemote = defaultRemoteName
	}
	fmt.Fprintf(f, "default_remote = %s\n", strconv.Quote(b.DefaultRemote))
	var names []string
	for name := range b.Remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		remote := b.Remotes[name]
		fmt.Fprintf(f, "\n[remotes.%s]\n", name)
		if remote.AirlockURL != "" {
			fmt.Fprintf(f, "url = %s\n", strconv.Quote(normalizeBaseURL(remote.AirlockURL)))
		}
		if remote.AgentID != "" {
			fmt.Fprintf(f, "agent_id = %s\n", strconv.Quote(remote.AgentID))
		}
		if remote.Slug != "" {
			fmt.Fprintf(f, "slug = %s\n", strconv.Quote(remote.Slug))
		}
		if remote.SourceState != "" {
			fmt.Fprintf(f, "source_state = %s\n", strconv.Quote(remote.SourceState))
		}
	}
	return nil
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

func (b *agentBinding) setRemote(name string, remote agentRemoteBinding) {
	if name == "" {
		name = b.DefaultRemote
	}
	if name == "" {
		name = defaultRemoteName
	}
	if b.Remotes == nil {
		b.Remotes = map[string]agentRemoteBinding{}
	}
	b.DefaultRemote = name
	b.Remotes[name] = remote
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
