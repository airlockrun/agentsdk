//go:build !linux && !windows

package connector

import (
	"context"
	"errors"
)

func runServiceIdentityValidation(context.Context, string, string, string, func(context.Context) error) error {
	return errors.New("connector: service identity validation is unsupported on this platform")
}
