package agentsdk

import (
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

//go:embed UPGRADING.md
var upgradeInstructionsMarkdown string

type upgradeInstruction struct {
	version string
	text    string
}

// UpgradeInstructions returns the SDK source-migration entries in the range
// fromVersion < entry version <= toVersion, ordered from oldest to newest.
// Versions may include or omit the leading "v". An empty result means the
// range has no documented source migrations.
func UpgradeInstructions(fromVersion, toVersion string) (string, error) {
	return selectUpgradeInstructions(upgradeInstructionsMarkdown, fromVersion, toVersion)
}

func selectUpgradeInstructions(markdown, fromVersion, toVersion string) (string, error) {
	from, err := normalizeUpgradeVersion(fromVersion)
	if err != nil {
		return "", fmt.Errorf("invalid source SDK version %q: %w", fromVersion, err)
	}
	to, err := normalizeUpgradeVersion(toVersion)
	if err != nil {
		return "", fmt.Errorf("invalid target SDK version %q: %w", toVersion, err)
	}
	if semver.Compare(from, to) > 0 {
		return "", fmt.Errorf("cannot select upgrade instructions for downgrade from %s to %s; rebuild with a target SDK at or above the source version", fromVersion, toVersion)
	}

	entries, err := parseUpgradeInstructions(markdown)
	if err != nil {
		return "", fmt.Errorf("parse embedded SDK upgrade instructions: %w", err)
	}
	var selected []string
	for _, entry := range entries {
		if semver.Compare(from, entry.version) < 0 && semver.Compare(entry.version, to) <= 0 {
			selected = append(selected, entry.text)
		}
	}
	return strings.Join(selected, "\n\n"), nil
}

func normalizeUpgradeVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return "", errors.New("must be a valid semantic version")
	}
	return version, nil
}

func parseUpgradeInstructions(markdown string) ([]upgradeInstruction, error) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var entries []upgradeInstruction
	var version string
	var body []string
	var fence byte
	var fenceLength int

	finish := func() error {
		if version == "" {
			return nil
		}
		content := strings.TrimSpace(strings.Join(body, "\n"))
		if content == "" {
			return fmt.Errorf("upgrade entry %s has no instructions", version)
		}
		entries = append(entries, upgradeInstruction{
			version: version,
			text:    "## " + version + "\n\n" + content,
		})
		return nil
	}

	for lineNumber, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			length := 0
			for length < len(trimmed) && trimmed[length] == trimmed[0] {
				length++
			}
			if length >= 3 {
				if fence == 0 {
					fence, fenceLength = trimmed[0], length
				} else if trimmed[0] == fence && length >= fenceLength && strings.TrimSpace(trimmed[length:]) == "" {
					fence, fenceLength = 0, 0
				}
			}
		}
		levelTwoHeading := indent <= 3 && strings.HasPrefix(trimmed, "##") && !strings.HasPrefix(trimmed, "###") &&
			(len(trimmed) == 2 || trimmed[2] == ' ' || trimmed[2] == '\t')
		if fence == 0 && levelTwoHeading {
			if err := finish(); err != nil {
				return nil, err
			}
			if indent != 0 || !strings.HasPrefix(line, "## ") {
				return nil, fmt.Errorf("line %d has malformed version heading %q", lineNumber+1, line)
			}
			raw := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if !strings.HasPrefix(raw, "v") {
				return nil, fmt.Errorf("line %d has malformed version heading %q", lineNumber+1, line)
			}
			normalized, err := normalizeUpgradeVersion(raw)
			if err != nil || raw == "" || strings.ContainsAny(raw, " \t") {
				return nil, fmt.Errorf("line %d has malformed version heading %q", lineNumber+1, line)
			}
			version = normalized
			body = nil
			continue
		}
		if version != "" {
			body = append(body, line)
		}
	}
	if fence != 0 {
		return nil, fmt.Errorf("unterminated fenced code block")
	}
	if err := finish(); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return semver.Compare(entries[i].version, entries[j].version) < 0
	})
	for i := 1; i < len(entries); i++ {
		if semver.Compare(entries[i-1].version, entries[i].version) == 0 {
			return nil, fmt.Errorf("duplicate upgrade version precedence %s and %s", entries[i-1].version, entries[i].version)
		}
	}
	return entries, nil
}
