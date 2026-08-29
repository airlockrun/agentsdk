//go:build !linux && !windows && !darwin

package connector

import (
	"context"
	"errors"
)

type unsupportedService struct{}

func newServiceManager(string, string, ServiceMode, Operations, []string) serviceManager {
	return unsupportedService{}
}
func (unsupportedService) PrepareIdentity(context.Context) error {
	return errors.New("connector: service identities are unsupported on this platform")
}
func (unsupportedService) ValidateIdentity(context.Context, string, string) error {
	return errors.New("connector: service identity validation is unsupported on this platform")
}
func (unsupportedService) Install(context.Context) error {
	return errors.New("connector: service installation is unsupported on this platform")
}
func (unsupportedService) Uninstall(context.Context) error {
	return errors.New("connector: service installation is unsupported on this platform")
}
func (unsupportedService) Start(context.Context) error {
	return errors.New("connector: services are unsupported on this platform")
}
func (unsupportedService) Stop(context.Context) error {
	return errors.New("connector: services are unsupported on this platform")
}
func (unsupportedService) Status(context.Context) (string, error) {
	return "unsupported", errors.New("connector: services are unsupported on this platform")
}
func (unsupportedService) Reconfigure(context.Context) (func() error, error) {
	return nil, errors.New("connector: service configuration is unsupported on this platform")
}
func (unsupportedService) Upgrade(context.Context, func() error) (bool, error) {
	return false, errors.New("connector: service upgrades are unsupported on this platform")
}
func (unsupportedService) Enable(context.Context) error {
	return errors.New("connector: services are unsupported on this platform")
}
func (unsupportedService) Disable(context.Context) error {
	return errors.New("connector: services are unsupported on this platform")
}
func (unsupportedService) Installed() bool { return false }
func (unsupportedService) Rollback(context.Context) error {
	return errors.New("connector: service rollback is unsupported on this platform")
}
func (unsupportedService) RollbackDigest() (string, error) {
	return "", errors.New("connector: service rollback is unsupported on this platform")
}
