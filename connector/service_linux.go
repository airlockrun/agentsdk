//go:build linux

package connector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type linuxService struct {
	kind, stateDir string
	mode           ServiceMode
	ops            Operations
	writableRoots  []string
}

func newServiceManager(kind, stateDir string, mode ServiceMode, operations Operations, writableRoots []string) serviceManager {
	return &linuxService{kind: kind, stateDir: stateDir, mode: mode, ops: operations, writableRoots: writableRoots}
}

func (s *linuxService) installationID() string {
	id := filepath.Base(s.stateDir)
	if validInstallationID(id) {
		return id
	}
	return ""
}
func (s *linuxService) name() string {
	name := "airlock-connector-" + s.kind
	if id := s.installationID(); id != "" {
		name += "-" + id
	}
	return name
}

func (s *linuxService) binary() string {
	if s.mode == ServiceSystem {
		return filepath.Join("/usr/local/lib/airlock-connectors", s.name())
	}
	return filepath.Join(s.stateDir, "bin", s.name())
}

func (s *linuxService) writeBinary(path string, body []byte, mode os.FileMode) error {
	if s.mode == ServiceSystem {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	return atomicWrite(path, body, mode)
}

func (s *linuxService) unitPath() string {
	if s.mode == ServiceUser {
		home, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		return filepath.Join(home, ".config", "systemd", "user", s.name()+".service")
	}
	return filepath.Join("/etc/systemd/system", s.name()+".service")
}

func (s *linuxService) systemctl(ctx context.Context, args ...string) error {
	if s.mode == ServiceUser {
		args = append([]string{"--user"}, args...)
	}
	_, err := s.ops.Execute(ctx, "systemctl", args...)
	return err
}

func (s *linuxService) PrepareIdentity(ctx context.Context) error {
	if s.mode == ServiceUser {
		return nil
	}
	if s.mode != ServiceSystem {
		return fmt.Errorf("connector: unsupported Linux service mode %q", s.mode)
	}
	for _, parent := range []string{filepath.Dir(s.stateDir), filepath.Dir(filepath.Dir(s.stateDir))} {
		info, err := os.Stat(parent)
		if err != nil {
			return fmt.Errorf("connector: inspect service state parent: %w", err)
		}
		if err := os.Chmod(parent, info.Mode().Perm()|0o111); err != nil {
			return fmt.Errorf("connector: make service state parent traversable: %w", err)
		}
	}
	account := s.account()
	if _, err := s.ops.Execute(ctx, "id", "-u", account); err != nil {
		if _, err := s.ops.Execute(ctx, "useradd", "--system", "--no-create-home", "--home-dir", s.stateDir, "--shell", "/usr/sbin/nologin", account); err != nil {
			return err
		}
	}
	if _, err := s.ops.Execute(ctx, "chown", "-R", account+":"+account, s.stateDir); err != nil {
		return err
	}
	return nil
}

func (s *linuxService) ValidateIdentity(ctx context.Context, installationID, settingsPath string) error {
	if s.mode != ServiceSystem {
		return errors.New("connector: service-identity validation requires system mode")
	}
	if err := s.PrepareIdentity(ctx); err != nil {
		return err
	}
	executable, err := s.ops.Executable()
	if err != nil {
		return err
	}
	args := []string{"-u", s.account(), "--", executable, "validate-service", "--identity", s.account()}
	if installationID != "" {
		args = append(args, "--installation", installationID)
	} else {
		args = append(args, "--draft")
	}
	if settingsPath != "" {
		args = append(args, "--settings-file", settingsPath)
	}
	envArgs := append([]string{"-u", "AIRLOCK_CONNECTOR_INSTALLATION_ID", "-u", "AIRLOCK_CONNECTOR_MODE", "/usr/sbin/runuser"}, args...)
	_, err = s.ops.Execute(ctx, "/usr/bin/env", envArgs...)
	if err != nil {
		return fmt.Errorf("connector: validate as service identity %s: %w", s.account(), err)
	}
	return nil
}

func (s *linuxService) Install(ctx context.Context) error {
	if s.mode != ServiceSystem && s.mode != ServiceUser {
		return fmt.Errorf("connector: unsupported Linux service mode %q", s.mode)
	}
	source, err := s.ops.Executable()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := s.writeBinary(s.binary(), body, 0o755); err != nil {
		return err
	}
	if s.mode == ServiceSystem {
		if err := s.PrepareIdentity(ctx); err != nil {
			return err
		}
	}
	unit, err := s.unit()
	if err != nil {
		return err
	}
	if err := atomicWrite(s.unitPath(), unit, 0o644); err != nil {
		return err
	}
	return s.systemctl(ctx, "daemon-reload")
}

func (s *linuxService) unit() ([]byte, error) {
	binary, err := systemdQuote(s.binary())
	if err != nil {
		return nil, err
	}
	writableRoots := append([]string{s.stateDir}, s.writableRoots...)
	writable := make([]string, 0, len(writableRoots))
	for _, root := range writableRoots {
		quoted, err := systemdQuote(root)
		if err != nil {
			return nil, err
		}
		writable = append(writable, quoted)
	}
	unit := "[Unit]\nDescription=Airlock connector " + s.kind + "\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nExecStart=" + binary + " run\nEnvironment=AIRLOCK_CONNECTOR_INSTALLATION_ID=" + s.installationID() + "\nRestart=on-failure\nNoNewPrivileges=true\nPrivateTmp=true\nProtectSystem=strict\nReadWritePaths=" + strings.Join(writable, " ") + "\n"
	wantedBy := "default.target"
	if s.mode == ServiceSystem {
		account := s.account()
		unit += "User=" + account + "\nGroup=" + account + "\n"
		wantedBy = "multi-user.target"
	}
	unit += "\n[Install]\nWantedBy=" + wantedBy + "\n"
	return []byte(unit), nil
}

func systemdQuote(value string) (string, error) {
	if !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("connector: systemd paths must be absolute and single-line")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\"", nil
}

func (s *linuxService) account() string {
	const prefix = "airlock-"
	digest := sha256.Sum256([]byte(s.kind + "\x00" + filepath.Clean(s.stateDir)))
	prefixPart := s.kind
	if len(prefixPart) > 12 {
		prefixPart = prefixPart[:12]
	}
	return prefix + prefixPart + fmt.Sprintf("-%x", digest[:5])
}

func (s *linuxService) Uninstall(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if err := s.systemctl(ctx, "disable", s.name()+".service"); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.binary()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.binary() + ".rollback"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.rollbackUnitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if s.mode == ServiceSystem {
		_, err := s.ops.Execute(ctx, "userdel", s.account())
		return err
	}
	return nil
}

func (s *linuxService) Start(ctx context.Context) error {
	if err := s.systemctl(ctx, "start", s.name()+".service"); err != nil {
		return err
	}
	status, err := s.Status(ctx)
	if err != nil || status != "active" {
		return errors.New("connector: systemd service did not become active")
	}
	return nil
}
func (s *linuxService) Stop(ctx context.Context) error {
	if err := s.systemctl(ctx, "stop", s.name()+".service"); err != nil {
		return err
	}
	args := []string{"is-active", s.name() + ".service"}
	if s.mode == ServiceUser {
		args = append([]string{"--user"}, args...)
	}
	body, err := s.ops.Execute(ctx, "systemctl", args...)
	status := strings.TrimSpace(string(body))
	if status == "inactive" || status == "failed" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("connector: systemd service did not stop: %w", err)
	}
	return errors.New("connector: systemd service remained active after stop")
}
func (s *linuxService) Enable(ctx context.Context) error {
	return s.systemctl(ctx, "enable", s.name()+".service")
}
func (s *linuxService) Disable(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	return s.systemctl(ctx, "disable", s.name()+".service")
}
func (s *linuxService) Status(ctx context.Context) (string, error) {
	args := []string{"is-active", s.name() + ".service"}
	if s.mode == ServiceUser {
		args = append([]string{"--user"}, args...)
	}
	body, err := s.ops.Execute(ctx, "systemctl", args...)
	return strings.TrimSpace(string(body)), err
}
func (s *linuxService) Reconfigure(ctx context.Context) (func() error, error) {
	previous, err := os.ReadFile(s.unitPath())
	if err != nil {
		return nil, err
	}
	unit, err := s.unit()
	if err != nil {
		return nil, err
	}
	restore := func() error {
		if err := atomicWrite(s.unitPath(), previous, 0o644); err != nil {
			return err
		}
		return s.systemctl(ctx, "daemon-reload")
	}
	if err := atomicWrite(s.unitPath(), unit, 0o644); err != nil {
		return nil, err
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}

func (s *linuxService) rollbackUnitPath() string {
	return s.binary() + ".rollback.service"
}

func (s *linuxService) Upgrade(ctx context.Context, activate func() error) (bool, error) {
	source, err := s.ops.Executable()
	if err != nil {
		return false, err
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	current, err := os.ReadFile(s.binary())
	if err != nil {
		return false, err
	}
	if err := s.writeBinary(s.binary()+".rollback", current, 0o600); err != nil {
		return false, err
	}
	currentUnit, err := os.ReadFile(s.unitPath())
	if err != nil {
		return false, err
	}
	if err := s.writeBinary(s.rollbackUnitPath(), currentUnit, 0o600); err != nil {
		return false, err
	}
	if err := s.Stop(ctx); err != nil {
		return true, err
	}
	if err := s.writeBinary(s.binary(), body, 0o755); err != nil {
		return true, err
	}
	if err := activate(); err != nil {
		return true, err
	}
	if _, err := s.Reconfigure(ctx); err != nil {
		return true, err
	}
	if err := s.Start(ctx); err != nil {
		return true, err
	}
	return true, nil
}
func (s *linuxService) Installed() bool {
	_, binaryErr := os.Stat(s.binary())
	_, unitErr := os.Stat(s.unitPath())
	return binaryErr == nil && unitErr == nil
}
func (s *linuxService) Rollback(ctx context.Context) error {
	rollback, err := os.ReadFile(s.binary() + ".rollback")
	if err != nil {
		return err
	}
	if err := s.writeBinary(s.binary(), rollback, 0o755); err != nil {
		return err
	}
	rollbackUnit, err := os.ReadFile(s.rollbackUnitPath())
	if err != nil {
		return err
	}
	if err := atomicWrite(s.unitPath(), rollbackUnit, 0o644); err != nil {
		return err
	}
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return s.Start(ctx)
}

func (s *linuxService) RollbackDigest() (string, error) {
	return fileDigest(s.binary() + ".rollback")
}
