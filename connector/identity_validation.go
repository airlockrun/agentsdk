package connector

import (
	"context"
	"errors"
	"flag"
	"path/filepath"
	"strings"
)

type serviceValidationResult struct {
	Error string `json:"error,omitempty"`
}

func (r *Runtime) validateServiceCommand(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("validate-service", flag.ContinueOnError)
	set.SetOutput(r.config.ErrorOutput)
	identity := set.String("identity", "", "expected service identity")
	serviceName := set.String("service-name", "", "temporary service name")
	resultPath := set.String("result", "", "validation result path")
	settingsFile := set.String("settings-file", "", "staged settings path")
	draft := set.Bool("draft", false, "validate unactivated draft state")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || strings.TrimSpace(*identity) == "" {
		return errors.New("connector: invalid service identity validation invocation")
	}
	if *draft && r.installationID != "" {
		return errors.New("connector: draft service validation cannot select an installation")
	}
	if *resultPath != "" {
		clean := filepath.Clean(*resultPath)
		if filepath.Dir(clean) != filepath.Clean(r.stateDir) || !strings.HasPrefix(filepath.Base(clean), ".service-validation-") || filepath.Ext(clean) != ".json" {
			return errors.New("connector: service validation result must be a private runtime state file")
		}
		*resultPath = clean
	}
	if *settingsFile != "" {
		clean := filepath.Clean(*settingsFile)
		if clean != filepath.Join(r.stateDir, ".upgrade-settings.json") {
			return errors.New("connector: service validation settings must be the staged upgrade file")
		}
		if err := loadSettings(clean, r.settingValues, r.machineState); err != nil {
			return err
		}
		if err := r.publishSettings(); err != nil {
			return err
		}
	}
	return runServiceIdentityValidation(ctx, *identity, *serviceName, *resultPath, r.validate)
}
