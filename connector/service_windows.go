//go:build windows

package connector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type windowsService struct {
	kind, stateDir string
	mode           ServiceMode
	ops            Operations
}

const windowsServiceWaitTimeout = 30 * time.Second

func newServiceManager(kind, stateDir string, mode ServiceMode, operations Operations, _ []string) serviceManager {
	return &windowsService{kind: kind, stateDir: stateDir, mode: mode, ops: operations}
}

func (s *windowsService) installationID() string {
	id := filepath.Base(s.stateDir)
	if validInstallationID(id) {
		return id
	}
	return ""
}
func (s *windowsService) name() string {
	name := "airlock-connector-" + s.kind
	if id := s.installationID(); id != "" {
		name += "-" + id
	}
	return name
}
func (s *windowsService) binary() string {
	if s.mode == ServiceSystem {
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			panic("connector: ProgramFiles is required for a Windows system service")
		}
		return filepath.Join(programFiles, "Airlock", "Connectors", s.name()+".exe")
	}
	return filepath.Join(s.stateDir, "bin", s.name()+".exe")
}

func (s *windowsService) PrepareIdentity(context.Context) error {
	if s.mode == ServiceUser {
		return nil
	}
	if s.mode != ServiceSystem {
		return fmt.Errorf("connector: unsupported Windows service mode %q", s.mode)
	}
	return setSystemStateACL(s.stateDir, s.account(), s.ops)
}

func (s *windowsService) account() string { return `NT SERVICE\` + s.name() }

func (s *windowsService) ValidateIdentity(ctx context.Context, installationID, settingsPath string) (returnErr error) {
	if s.mode != ServiceSystem {
		return errors.New("connector: service-identity validation requires system mode")
	}
	source, err := s.ops.Executable()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(s.stateDir))
	suffix := fmt.Sprintf("%x", digest[:6])
	name := "airlock-connector-validation-" + suffix
	validationAccount := `NT SERVICE\` + name
	validationBinary := filepath.Join(filepath.Dir(s.binary()), ".validate-"+suffix+".exe")
	resultPath := filepath.Join(s.stateDir, ".service-validation-"+suffix+".json")
	if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWrite(validationBinary, body, 0o700); err != nil {
		return err
	}
	restoreAccount := ""
	if s.Installed() {
		restoreAccount = s.account()
	}
	defer func() {
		returnErr = errors.Join(returnErr, setSystemStateACL(s.stateDir, restoreAccount, s.ops))
		if err := os.Remove(validationBinary); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	commandLine, err := windowsValidationCommand(validationBinary, name, resultPath, installationID, settingsPath)
	if err != nil {
		return err
	}
	if _, err := s.ops.Execute(ctx, "sc.exe", "create", name, "binPath=", commandLine, "start=", "demand", "obj=", validationAccount); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), windowsServiceWaitTimeout)
		defer cancel()
		_, err := s.ops.Execute(cleanupCtx, "sc.exe", "delete", name)
		returnErr = errors.Join(returnErr, err)
	}()
	if err := setSystemStateACL(s.stateDir, validationAccount, s.ops); err != nil {
		return err
	}
	if _, err := s.ops.Execute(ctx, "sc.exe", "start", name); err != nil {
		return err
	}
	if err := s.waitNamedSC(ctx, name, "STOPPED"); err != nil {
		return err
	}
	encoded, err := os.ReadFile(resultPath)
	if err != nil {
		return fmt.Errorf("connector: service identity validation produced no result: %w", err)
	}
	defer func() {
		if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	var result serviceValidationResult
	if err := strictUnmarshal(encoded, &result); err != nil {
		return fmt.Errorf("connector: decode service identity validation: %w", err)
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}

func windowsValidationCommand(binary, name, resultPath, installationID, settingsPath string) (string, error) {
	for _, value := range []string{binary, name, resultPath, installationID, settingsPath} {
		if strings.ContainsAny(value, "\"\r\n") {
			return "", errors.New("connector: Windows service validation arguments cannot contain quotes or newlines")
		}
	}
	command := `"` + binary + `" validate-service --identity "NT SERVICE\` + name + `" --service-name "` + name + `" --result "` + resultPath + `"`
	if installationID != "" {
		command += " --installation " + installationID
	} else {
		command += " --draft"
	}
	if settingsPath != "" {
		command += ` --settings-file "` + settingsPath + `"`
	}
	return command, nil
}

func (s *windowsService) Install(ctx context.Context) error {
	source, err := s.ops.Executable()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := atomicWrite(s.binary(), body, 0o700); err != nil {
		return err
	}
	if s.mode == ServiceUser {
		_, err = s.ops.Execute(ctx, "schtasks.exe", "/Create", "/F", "/SC", "ONLOGON", "/TN", s.name(), "/TR", `"`+s.binary()+`" run --installation `+s.installationID())
		return err
	}
	if s.mode != ServiceSystem {
		return fmt.Errorf("connector: unsupported Windows service mode %q", s.mode)
	}
	_, err = s.ops.Execute(ctx, "sc.exe", "create", s.name(), "binPath=", `"`+s.binary()+`" run`, "start=", "auto", "obj=", s.account())
	if err != nil {
		return err
	}
	if err := s.PrepareIdentity(ctx); err != nil {
		_, _ = s.ops.Execute(ctx, "sc.exe", "delete", s.name())
		return err
	}
	_, err = s.ops.Execute(ctx, "reg.exe", "add", `HKLM\SYSTEM\CurrentControlSet\Services\`+s.name(), "/v", "Environment", "/t", "REG_MULTI_SZ", "/d", "AIRLOCK_CONNECTOR_INSTALLATION_ID="+s.installationID(), "/f")
	if err != nil {
		_, _ = s.ops.Execute(ctx, "sc.exe", "delete", s.name())
		return err
	}
	return nil
}

func (s *windowsService) Uninstall(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	var err error
	if s.mode == ServiceUser {
		_, err = s.ops.Execute(ctx, "schtasks.exe", "/Delete", "/F", "/TN", s.name())
	} else {
		_, err = s.ops.Execute(ctx, "sc.exe", "delete", s.name())
	}
	if err != nil {
		return err
	}
	if s.mode != ServiceUser {
		waitCtx, cancel := context.WithTimeout(ctx, windowsServiceWaitTimeout)
		defer cancel()
		for {
			body, queryErr := s.ops.Execute(waitCtx, "sc.exe", "query", s.name())
			if queryErr != nil && (strings.Contains(strings.ToLower(string(body)), "does not exist") || strings.Contains(queryErr.Error(), "1060")) {
				break
			}
			if queryErr != nil {
				return fmt.Errorf("connector: query Windows service deletion: %w", queryErr)
			}
			select {
			case <-waitCtx.Done():
				return fmt.Errorf("connector: Windows service deletion timeout: %w", waitCtx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	if err := os.Remove(s.binary()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.binary() + ".rollback"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *windowsService) Start(ctx context.Context) error {
	if s.mode == ServiceUser {
		_, err := s.ops.Execute(ctx, "schtasks.exe", "/Run", "/TN", s.name())
		return err
	}
	_, err := s.ops.Execute(ctx, "sc.exe", "start", s.name())
	if err != nil {
		return err
	}
	return s.waitSC(ctx, "RUNNING")
}
func (s *windowsService) Stop(ctx context.Context) error {
	if s.mode == ServiceUser {
		body, err := s.ops.Execute(ctx, "schtasks.exe", "/End", "/TN", s.name())
		if err == nil || windowsTaskNotRunning(body, err) {
			return nil
		}
		status, queryErr := s.ops.Execute(ctx, "schtasks.exe", "/Query", "/TN", s.name(), "/FO", "LIST", "/V")
		if windowsTaskNotRunning(status, queryErr) || queryErr == nil && windowsTaskStopped(status) {
			return nil
		}
		return err
	}
	body, err := s.ops.Execute(ctx, "sc.exe", "stop", s.name())
	if err != nil {
		if windowsServiceStoppedOrMissing(body, err) {
			return nil
		}
		body, queryErr := s.ops.Execute(ctx, "sc.exe", "query", s.name())
		if windowsServiceStoppedOrMissing(body, queryErr) {
			return nil
		}
		return err
	}
	return s.waitSC(ctx, "STOPPED")
}

func windowsTaskNotRunning(body []byte, err error) bool {
	message := strings.ToLower(string(body))
	if err != nil {
		message += " " + strings.ToLower(err.Error())
	}
	for _, marker := range []string{"not currently running", "cannot find the file", "cannot find the task", "0x80070002", "0x8004130b", "267011", "0x41303"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func windowsTaskStopped(body []byte) bool {
	message := strings.ToLower(string(body))
	return strings.Contains(message, "status:") && (strings.Contains(message, "ready") || strings.Contains(message, "disabled"))
}

func windowsServiceStoppedOrMissing(body []byte, err error) bool {
	message := strings.ToLower(string(body))
	if err != nil {
		message += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(message, "stopped") || strings.Contains(message, "1060") || strings.Contains(message, "1062") || strings.Contains(message, "does not exist")
}
func (s *windowsService) Enable(ctx context.Context) error {
	if s.mode == ServiceUser {
		_, err := s.ops.Execute(ctx, "schtasks.exe", "/Change", "/ENABLE", "/TN", s.name())
		return err
	}
	_, err := s.ops.Execute(ctx, "sc.exe", "config", s.name(), "start=", "auto")
	return err
}
func (s *windowsService) Disable(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if s.mode == ServiceUser {
		_, err := s.ops.Execute(ctx, "schtasks.exe", "/Change", "/DISABLE", "/TN", s.name())
		return err
	}
	_, err := s.ops.Execute(ctx, "sc.exe", "config", s.name(), "start=", "disabled")
	return err
}
func (s *windowsService) Status(ctx context.Context) (string, error) {
	var body []byte
	var err error
	if s.mode == ServiceUser {
		body, err = s.ops.Execute(ctx, "schtasks.exe", "/Query", "/TN", s.name())
	} else {
		body, err = s.ops.Execute(ctx, "sc.exe", "query", s.name())
	}
	return strings.TrimSpace(string(body)), err
}
func (s *windowsService) Reconfigure(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}
func (s *windowsService) Upgrade(ctx context.Context, activate func() error) (bool, error) {
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
	if err := atomicWrite(s.binary()+".rollback", current, 0o700); err != nil {
		return false, err
	}
	if err := s.Stop(ctx); err != nil {
		return true, err
	}
	if err := atomicWrite(s.binary(), body, 0o700); err != nil {
		return true, err
	}
	if err := activate(); err != nil {
		return true, err
	}
	if err := s.Start(ctx); err != nil {
		return true, err
	}
	return true, nil
}
func (s *windowsService) Installed() bool { _, err := os.Stat(s.binary()); return err == nil }
func (s *windowsService) Rollback(ctx context.Context) error {
	rollback, err := os.ReadFile(s.binary() + ".rollback")
	if err != nil {
		return err
	}
	if err := atomicWrite(s.binary(), rollback, 0o700); err != nil {
		return err
	}
	return s.Start(ctx)
}
func (s *windowsService) RollbackDigest() (string, error) {
	return fileDigest(s.binary() + ".rollback")
}
func (s *windowsService) waitSC(ctx context.Context, wanted string) error {
	return s.waitNamedSC(ctx, s.name(), wanted)
}
func (s *windowsService) waitNamedSC(ctx context.Context, name, wanted string) error {
	waitCtx, cancel := context.WithTimeout(ctx, windowsServiceWaitTimeout)
	defer cancel()
	for {
		body, err := s.ops.Execute(waitCtx, "sc.exe", "query", name)
		if err != nil {
			return fmt.Errorf("connector: query Windows service while waiting for %s: %w", wanted, err)
		}
		state := windowsServiceState(body)
		if state == wanted {
			return nil
		}
		if state != "" && !windowsServiceCanTransition(state, wanted) {
			return fmt.Errorf("connector: Windows service reached terminal state %s while waiting for %s", state, wanted)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("connector: Windows service did not reach %s: %w", wanted, waitCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func windowsServiceCanTransition(state, wanted string) bool {
	switch wanted {
	case "RUNNING":
		return state == "START_PENDING" || state == "CONTINUE_PENDING"
	case "STOPPED":
		return state == "START_PENDING" || state == "RUNNING" || state == "STOP_PENDING"
	default:
		return false
	}
}

func windowsServiceState(body []byte) string {
	upper := strings.ToUpper(string(body))
	for _, state := range []string{"CONTINUE_PENDING", "PAUSE_PENDING", "START_PENDING", "STOP_PENDING", "RUNNING", "STOPPED", "PAUSED"} {
		if strings.Contains(upper, state) {
			return state
		}
	}
	return ""
}
