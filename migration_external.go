package agentsdk

import (
	"context"
	"errors"
)

// MigrationExternalStep runs an external migration operation with the Agent
// for the current migration. Validation skips the callback because storage,
// credentials, and third-party services are unavailable in that mode.
//
// Goose does not record completion of a NoTx migration atomically with an
// external service. Production callbacks have at-least-once execution
// semantics: if a process stops after the callback succeeds, goose may invoke
// it again on the next startup. The callback must therefore be repeat-safe;
// MigrationExternalStep does not retry or checkpoint it.
func MigrationExternalStep(ctx context.Context, step func(context.Context, *Agent) error) error {
	if step == nil {
		panic("agentsdk: MigrationExternalStep requires a callback")
	}
	if isValidatingMigrations() {
		return nil
	}
	return step(ctx, agentFromMigrationContext(ctx))
}

// MoveFile repeat-safely moves an object in agent storage for operational
// migrations. If src exists it is copied to dst before src is deleted. If src
// is absent and dst exists, the move is already complete. If neither exists,
// MoveFile returns ErrNotFound.
func (a *Agent) MoveFile(ctx context.Context, src, dst string) error {
	if !a.runtimeAvailable() {
		return a.runtimeUnavailable("MoveFile")
	}
	_, srcErr := a.StatFile(ctx, src)
	_, dstErr := a.StatFile(ctx, dst)
	if srcErr != nil && !errors.Is(srcErr, ErrNotFound) {
		return srcErr
	}
	if dstErr != nil && !errors.Is(dstErr, ErrNotFound) {
		return dstErr
	}
	if errors.Is(srcErr, ErrNotFound) {
		if dstErr == nil {
			return nil
		}
		return ErrNotFound
	}
	if err := a.CopyFile(ctx, src, dst); err != nil {
		return err
	}
	if err := a.DeleteFile(ctx, src); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}
