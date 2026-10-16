package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const agentBindingPath = ".airlock/local/agent.toml"
const defaultRemoteName = "default"
const bindingSchema = "targets-v1"

type agentBinding struct {
	DefaultRemote string
	Remotes       map[string]agentRemoteBinding
}

type agentRemoteBinding struct {
	AirlockURL         string
	AgentID            string
	Slug               string
	SourceETag         string
	SourceState        string
	DeveloperSession   string
	PendingTask        string
	PendingSourceState string
	Targets            map[string]targetSourceBinding
	Previews           map[string]previewReference
}

// A reference selects a target; it is not its ownership or authorization record.
type previewReference struct{ DeploymentID, Name string }

// Targets are keyed by the server-issued concrete runtime ID, so personal
// previews belonging to different logins never share source synchronization.
type targetSourceBinding struct {
	Name, OwnerUserID, SourceETag, SourceState, PendingTask, PendingSourceState string
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
	currentTarget := ""
	currentPreview := ""
	schema := ""
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
			section := strings.Split(strings.TrimPrefix(name, "remotes."), ".")
			currentRemote, currentTarget, currentPreview = section[0], "", ""
			if len(section) != 1 {
				if len(section) != 3 || (section[1] != "targets" && section[1] != "previews") || !deployUUIDRe.MatchString(section[2]) {
					return agentBinding{}, false, fmt.Errorf("%s: invalid target section %q", path, name)
				}
				if section[1] == "targets" {
					currentTarget = section[2]
				} else {
					currentPreview = section[2]
				}
			}
			if !validRemoteName(currentRemote) {
				return agentBinding{}, false, fmt.Errorf("%s: invalid remote name %q", path, currentRemote)
			}
			if seenSections[name] {
				return agentBinding{}, false, fmt.Errorf("%s: duplicate remote section %q", path, currentRemote)
			}
			seenSections[name] = true
			if _, exists := b.Remotes[currentRemote]; !exists {
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
		scopedKey := currentRemote + "\x00" + currentTarget + "\x00" + currentPreview + "\x00" + key
		if seenKeys[scopedKey] {
			return agentBinding{}, false, fmt.Errorf("%s: duplicate key %q", path, key)
		}
		seenKeys[scopedKey] = true
		if currentRemote == "" {
			switch key {
			case "default_remote":
				b.DefaultRemote = unquoted
			case "schema":
				schema = unquoted
			default:
				return agentBinding{}, false, fmt.Errorf("%s: unknown top-level key %q", path, key)
			}
			continue
		}
		remote := b.Remotes[currentRemote]
		if currentPreview != "" {
			if remote.Previews == nil {
				remote.Previews = map[string]previewReference{}
			}
			ref := remote.Previews[currentPreview]
			switch key {
			case "deployment_id":
				ref.DeploymentID = unquoted
			case "name":
				ref.Name = unquoted
			default:
				return agentBinding{}, false, fmt.Errorf("%s: unknown preview key %q", path, key)
			}
			remote.Previews[currentPreview] = ref
			b.Remotes[currentRemote] = remote
			continue
		}
		if currentTarget != "" {
			if remote.Targets == nil {
				remote.Targets = map[string]targetSourceBinding{}
			}
			target := remote.Targets[currentTarget]
			switch key {
			case "name":
				target.Name = unquoted
			case "owner_user_id":
				target.OwnerUserID = unquoted
			case "source_etag":
				target.SourceETag = unquoted
			case "source_state":
				target.SourceState = unquoted
			case "pending_task":
				target.PendingTask = unquoted
			case "pending_source_state":
				target.PendingSourceState = unquoted
			default:
				return agentBinding{}, false, fmt.Errorf("%s: unknown target key %q", path, key)
			}
			remote.Targets[currentTarget] = target
			b.Remotes[currentRemote] = remote
			continue
		}
		switch key {
		case "url":
			remote.AirlockURL = normalizeBaseURL(unquoted)
		case "agent_id":
			remote.AgentID = unquoted
		case "slug":
			remote.Slug = unquoted
		case "source_state":
			remote.SourceState = unquoted
		case "source_etag":
			remote.SourceETag = unquoted
		case "developer_session":
			remote.DeveloperSession = unquoted
		case "pending_task":
			remote.PendingTask = unquoted
		case "pending_source_state":
			remote.PendingSourceState = unquoted
		default:
			return agentBinding{}, false, fmt.Errorf("%s: unknown key %q", path, key)
		}
		b.Remotes[currentRemote] = remote
	}
	if err := s.Err(); err != nil {
		return agentBinding{}, false, err
	}
	if schema != bindingSchema {
		return agentBinding{}, false, fmt.Errorf("%s: unsupported alpha binding schema", path)
	}
	for _, remote := range b.Remotes {
		for _, ref := range remote.Previews {
			if !deployUUIDRe.MatchString(ref.DeploymentID) || !validRemoteName(ref.Name) || ref.Name == "preview" || ref.Name == "production" {
				return agentBinding{}, false, fmt.Errorf("%s: invalid selected preview reference", path)
			}
		}
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
		remote := b.Remotes[name]
		for _, ref := range remote.Previews {
			if !deployUUIDRe.MatchString(ref.DeploymentID) || !validRemoteName(ref.Name) || ref.Name == "preview" || ref.Name == "production" {
				return errors.New("invalid preview reference")
			}
		}
	}
	var content strings.Builder
	fmt.Fprintf(&content, "schema = %s\ndefault_remote = %s\n", strconv.Quote(bindingSchema), strconv.Quote(b.DefaultRemote))
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
		if remote.SourceETag != "" {
			fmt.Fprintf(&content, "source_etag = %s\n", strconv.Quote(remote.SourceETag))
		}
		if remote.DeveloperSession != "" {
			fmt.Fprintf(&content, "developer_session = %s\n", strconv.Quote(remote.DeveloperSession))
		}
		if remote.PendingTask != "" {
			fmt.Fprintf(&content, "pending_task = %s\n", strconv.Quote(remote.PendingTask))
		}
		if remote.PendingSourceState != "" {
			fmt.Fprintf(&content, "pending_source_state = %s\n", strconv.Quote(remote.PendingSourceState))
		}
		var ids []string
		for id := range remote.Targets {
			if !deployUUIDRe.MatchString(id) {
				return fmt.Errorf("%s: invalid runtime target ID %q", agentBindingPath, id)
			}
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			target := remote.Targets[id]
			fmt.Fprintf(&content, "\n[remotes.%s.targets.%s]\n", name, id)
			for _, field := range []struct{ key, value string }{
				{"name", target.Name}, {"owner_user_id", target.OwnerUserID}, {"source_etag", target.SourceETag},
				{"source_state", target.SourceState}, {"pending_task", target.PendingTask}, {"pending_source_state", target.PendingSourceState},
			} {
				if field.value != "" {
					fmt.Fprintf(&content, "%s = %s\n", field.key, strconv.Quote(field.value))
				}
			}
		}
		var apps []string
		for id := range remote.Previews {
			if !deployUUIDRe.MatchString(id) {
				return fmt.Errorf("invalid selected app ID %q", id)
			}
			apps = append(apps, id)
		}
		sort.Strings(apps)
		for _, id := range apps {
			ref := remote.Previews[id]
			fmt.Fprintf(&content, "\n[remotes.%s.previews.%s]\ndeployment_id = %s\nname = %s\n", name, id, strconv.Quote(ref.DeploymentID), strconv.Quote(ref.Name))
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
	if name == "" || len(name) > 128 {
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
