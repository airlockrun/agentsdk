package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const agentBindingPath = ".airlock/agent.toml"

type agentBinding struct {
	AirlockURL string
	AgentID    string
	Slug       string
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

	var b agentBinding
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
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
		switch key {
		case "airlock_url":
			b.AirlockURL = normalizeBaseURL(unquoted)
		case "agent_id":
			b.AgentID = unquoted
		case "slug":
			b.Slug = unquoted
		default:
			return agentBinding{}, false, fmt.Errorf("%s: unknown key %q", path, key)
		}
	}
	if err := s.Err(); err != nil {
		return agentBinding{}, false, err
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
	if b.AirlockURL != "" {
		fmt.Fprintf(f, "airlock_url = %s\n", strconv.Quote(normalizeBaseURL(b.AirlockURL)))
	}
	if b.AgentID != "" {
		fmt.Fprintf(f, "agent_id = %s\n", strconv.Quote(b.AgentID))
	}
	if b.Slug != "" {
		fmt.Fprintf(f, "slug = %s\n", strconv.Quote(b.Slug))
	}
	return nil
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
