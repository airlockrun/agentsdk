package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/airlockrun/agentsdk/wire"
)

// checkAppManifest inspects the build artifact without starting the SDK runtime.
func checkAppManifest(dir, binary string, warnings io.Writer) error {
	body, err := inspectManifestBinary(dir, binary, "AIRLOCK_AGENT_MODE=manifest", "app", 16<<20)
	if err != nil {
		return fmt.Errorf("inspect app manifest: %w", err)
	}
	var manifest wire.AgentManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("decode app manifest: %w", err)
	}
	if err := wire.CheckAppRuntimeProtocol(manifest.RuntimeProtocol); err != nil {
		return fmt.Errorf("app manifest: %w", err)
	}
	if manifest.Version == "" || manifest.Description == "" {
		return errors.New("app manifest requires version and description")
	}
	if len(manifest.Tools) == 0 {
		_, err := fmt.Fprintln(warnings, "WARNING: No tools registered. Chat cannot invoke this app’s domain actions. Register typed tools with RegisterTool for core workflows; intentional UI-only apps can ignore this warning.")
		return err
	}
	return nil
}
