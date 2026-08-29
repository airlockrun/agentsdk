//go:build darwin

package connector

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

func runAsService(ctx context.Context, _ string, run func(context.Context) error) error {
	serviceCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(serviceCtx)
	if ctx.Err() == nil && serviceCtx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
