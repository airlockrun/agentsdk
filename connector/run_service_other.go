//go:build !linux && !windows && !darwin

package connector

import "context"

func runAsService(ctx context.Context, _ string, run func(context.Context) error) error {
	return run(ctx)
}
