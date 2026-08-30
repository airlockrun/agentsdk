package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

var connectorSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type connectorTarget struct {
	slug        string
	packagePath string
}

func discoverConnectors(projectDir string) ([]connectorTarget, error) {
	root := filepath.Join(projectDir, "connectors")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("connectors must be a real directory, not a file or symlink")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	targets := make([]connectorTarget, 0, len(entries))
	for _, entry := range entries {
		entryInfo, err := os.Lstat(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("inspect connectors/%s: %w", entry.Name(), err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
			return nil, fmt.Errorf("connectors/%s is not a directory; every immediate child is a connector target", entry.Name())
		}
		if !connectorSlugPattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("connector slug %q must contain lowercase letters, digits, and internal hyphens", entry.Name())
		}
		targets = append(targets, connectorTarget{slug: entry.Name(), packagePath: "./connectors/" + entry.Name()})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].slug < targets[j].slug })
	return targets, nil
}

func buildConnectors(projectDir, outputDir string) error {
	targets, err := discoverConnectors(projectDir)
	if err != nil {
		return fmt.Errorf("discover connectors: %w", err)
	}
	for _, target := range targets {
		fmt.Printf("==> connector %s: validate main package\n", target.slug)
		name, err := connectorCommand(projectDir, nil, "go", "list", "-f", "{{.Name}}", target.packagePath)
		if err != nil {
			return fmt.Errorf("connector %s: %w", target.slug, err)
		}
		if strings.TrimSpace(string(name)) != "main" {
			return fmt.Errorf("connector %s: package must be main", target.slug)
		}
		native := filepath.Join(outputDir, "connector-"+target.slug)
		if runtime.GOOS == "windows" {
			native += ".exe"
		}
		fmt.Printf("==> connector %s: native manifest build\n", target.slug)
		if _, err := connectorCommand(projectDir, []string{"CGO_ENABLED=0"}, "go", "build", "-buildvcs=false", "-o", native, target.packagePath); err != nil {
			return fmt.Errorf("connector %s native build: %w", target.slug, err)
		}
		manifestBody, err := inspectConnectorManifest(projectDir, native)
		if err != nil {
			return fmt.Errorf("connector %s manifest: %w", target.slug, err)
		}
		var manifest protocol.Manifest
		decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(manifestBody), 4<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			return fmt.Errorf("connector %s manifest JSON: %w", target.slug, err)
		}
		if err := protocol.ValidateManifest(manifest); err != nil {
			return fmt.Errorf("connector %s manifest validation: %w", target.slug, err)
		}
		if manifest.Interface.Kind != target.slug {
			return fmt.Errorf("connector %s manifest kind is %q; kind must match its directory slug", target.slug, manifest.Interface.Kind)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return fmt.Errorf("connector %s manifest contains trailing JSON", target.slug)
		}
		for _, platform := range manifest.Targets {
			buildTarget, err := connectorBuildTarget(platform)
			if err != nil {
				return fmt.Errorf("connector %s target %s: %w", target.slug, platform, err)
			}
			artifact := filepath.Join(outputDir, target.slug+"-"+platform+buildTarget.ExecutableSuffix)
			fmt.Printf("==> connector %s: %s (CGO_ENABLED=0)\n", target.slug, platform)
			environment := append([]string{"CGO_ENABLED=0"}, buildTarget.GoEnv()...)
			if _, err := connectorCommand(projectDir, environment, "go", "build", "-buildvcs=false", "-o", artifact, target.packagePath); err != nil {
				return fmt.Errorf("connector %s target %s: %w", target.slug, platform, err)
			}
			body, err := os.ReadFile(artifact)
			if err != nil {
				return fmt.Errorf("connector %s target %s checksum: %w", target.slug, platform, err)
			}
			fmt.Printf("    sha256:%x\n", sha256.Sum256(body))
		}
	}
	return nil
}

func connectorBuildTarget(platform string) (protocol.Target, error) {
	target, ok := protocol.LookupTarget(platform)
	if !ok {
		return protocol.Target{}, fmt.Errorf("unsupported connector build target %q", platform)
	}
	return target, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining < len(value) {
		b.overflow = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = b.Buffer.Write(value)
	return original, nil
}

func inspectConnectorManifest(dir, binary string) ([]byte, error) {
	stdout := &limitedBuffer{limit: protocol.MaxManifestBytes + 1}
	stderr := &limitedBuffer{limit: 64 << 10}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	configureManifestProcess(command)
	defer terminateManifestDescendants(command)
	command.Dir = dir
	command.Env = connectorEnvironment([]string{"AIRLOCK_CONNECTOR_MODE=manifest"})
	command.Stdout, command.Stderr = stdout, stderr
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("connector manifest timed out after 30 seconds")
		}
		if stderr.overflow {
			return nil, errors.New("connector manifest stderr exceeds 64 KiB")
		}
		if stdout.overflow {
			return nil, fmt.Errorf("connector manifest stdout exceeds %d bytes", protocol.MaxManifestBytes)
		}
		return nil, fmt.Errorf("run manifest binary: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stderr.overflow {
		return nil, errors.New("connector manifest stderr exceeds 64 KiB")
	}
	if stdout.overflow || stdout.Len() > protocol.MaxManifestBytes {
		return nil, fmt.Errorf("connector manifest stdout exceeds %d bytes", protocol.MaxManifestBytes)
	}
	if stdout.Len() == 0 {
		return nil, errors.New("connector manifest stdout is empty")
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func connectorCommand(dir string, environment []string, name string, args ...string) ([]byte, error) {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Env = connectorEnvironment(environment)
	body, err := command.CombinedOutput()
	if err != nil {
		return body, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func connectorEnvironment(overrides []string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		values[key] = entry
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			panic("connector build environment override has no equals sign")
		}
		values[key] = entry
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, values[key])
	}
	return result
}
