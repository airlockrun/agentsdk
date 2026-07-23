// Package bootstrap validates the temporary module used to select the Air CLI
// before an agent repository exists.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	// ModulePath marks a directory as an Airlock bootstrap module.
	ModulePath = "airlock.bootstrap"
	// ToolImport is the repository-local Air CLI package.
	ToolImport = "github.com/airlockrun/agentsdk/cmd/air"
	goVersion  = "1.26.0"
)

// EnsureDir creates an absent directory and accepts only an empty directory or
// an exact Airlock bootstrap module. It reports whether bootstrap files exist.
func EnsureDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return false, fmt.Errorf("create %s: %w", dir, err)
			}
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return false, nil
	}

	for _, entry := range entries {
		if (entry.Name() != "go.mod" && entry.Name() != "go.sum") || !entry.Type().IsRegular() {
			return false, fmt.Errorf("target directory %s is not empty", dir)
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("target directory %s is not empty", dir)
		}
		return false, fmt.Errorf("read bootstrap go.mod: %w", err)
	}
	mf, err := modfile.Parse(filepath.Join(dir, "go.mod"), body, nil)
	if err != nil {
		return false, fmt.Errorf("parse bootstrap go.mod: %w", err)
	}
	if mf.Module == nil || mf.Module.Mod.Path != ModulePath {
		return false, fmt.Errorf("target directory %s is not empty: module path must be %s", dir, ModulePath)
	}
	if mf.Go == nil || len(mf.Tool) != 1 || mf.Tool[0].Path != ToolImport || mf.Toolchain != nil || len(mf.Godebug) != 0 ||
		len(mf.Exclude) != 0 || len(mf.Replace) != 0 || len(mf.Retract) != 0 || len(mf.Ignore) != 0 {
		return false, fmt.Errorf("target directory %s is not empty: bootstrap go.mod has unsupported directives", dir)
	}
	hasAgentSDK := false
	for _, require := range mf.Require {
		if !semver.IsValid(require.Mod.Version) {
			return false, fmt.Errorf("target directory %s is not empty: bootstrap go.mod has an invalid requirement", dir)
		}
		if require.Mod.Path == "github.com/airlockrun/agentsdk" {
			if hasAgentSDK {
				return false, fmt.Errorf("target directory %s is not empty: bootstrap go.mod repeats the Agent SDK requirement", dir)
			}
			hasAgentSDK = true
			continue
		}
		if !require.Indirect {
			return false, fmt.Errorf("target directory %s is not empty: bootstrap go.mod has an unsupported direct requirement", dir)
		}
	}
	if !hasAgentSDK {
		return false, fmt.Errorf("target directory %s is not empty: bootstrap go.mod does not require the Agent SDK", dir)
	}
	return true, nil
}

// InitializeDir writes a complete bootstrap go.mod into an empty directory.
// A complete file remains safe to retry if dependency resolution is interrupted.
func InitializeDir(dir, version string) error {
	isBootstrap, err := EnsureDir(dir)
	if err != nil {
		return err
	}
	if isBootstrap {
		return nil
	}
	body := fmt.Sprintf("module %s\n\ngo %s\n\nrequire github.com/airlockrun/agentsdk %s\n\ntool %s\n", ModulePath, goVersion, version, ToolImport)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write bootstrap go.mod: %w", err)
	}
	return nil
}

// ResetDir removes a validated bootstrap module so a scaffold can replace it.
func ResetDir(dir string) error {
	isBootstrap, err := EnsureDir(dir)
	if err != nil {
		return err
	}
	if !isBootstrap {
		return nil
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove bootstrap %s: %w", name, err)
		}
	}
	return nil
}
