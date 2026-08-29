//go:build linux

package connector

import (
	"context"
	"errors"
	"fmt"
	"os/user"
)

func runServiceIdentityValidation(ctx context.Context, identity, serviceName, resultPath string, validate func(context.Context) error) error {
	if serviceName != "" || resultPath != "" {
		return errors.New("connector: Linux service validation does not accept Windows service arguments")
	}
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("connector: resolve service validation identity: %w", err)
	}
	if current.Username != identity {
		return fmt.Errorf("connector: service validation is running as %q, want %q", current.Username, identity)
	}
	return validate(ctx)
}
