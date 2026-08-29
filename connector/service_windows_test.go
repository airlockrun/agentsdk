//go:build windows

package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scriptedWindowsOperations struct {
	executable string
	calls      []string
	run        func(string, ...string) ([]byte, error)
}

func (o *scriptedWindowsOperations) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	o.calls = append(o.calls, name+" "+strings.Join(args, " "))
	return o.run(name, args...)
}
func (o *scriptedWindowsOperations) Executable() (string, error) { return o.executable, nil }

func TestWindowsSystemServiceBinaryIsOutsideWritableState(t *testing.T) {
	programFiles := filepath.Join(`C:\`, "Program Files")
	t.Setenv("ProgramFiles", programFiles)
	stateDir := filepath.Join(`C:\ProgramData`, "Airlock", "Connectors", "sample", "installations", "00000000-0000-0000-0000-000000000001")
	service := newServiceManager("sample", stateDir, ServiceSystem, &windowsTestOperations{}, nil).(*windowsService)
	if strings.HasPrefix(strings.ToLower(service.binary()), strings.ToLower(stateDir+string(filepath.Separator))) {
		t.Fatalf("system service binary %q is beneath writable state %q", service.binary(), stateDir)
	}
	want := filepath.Join(programFiles, "Airlock", "Connectors", "airlock-connector-sample-00000000-0000-0000-0000-000000000001.exe")
	if service.binary() != want {
		t.Fatalf("binary = %q, want %q", service.binary(), want)
	}
}

func TestWindowsValidationCommandSelectsInstallation(t *testing.T) {
	command, err := windowsValidationCommand(`C:\Program Files\Airlock\connector.exe`, "airlock-validation", `C:\ProgramData\Airlock\result.json`, "00000000-0000-0000-0000-000000000001", `C:\ProgramData\Airlock\.upgrade-settings.json`)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`validate-service`, `--identity "NT SERVICE\airlock-validation"`, `--service-name "airlock-validation"`, `--installation 00000000-0000-0000-0000-000000000001`, `--settings-file "C:\ProgramData\Airlock\.upgrade-settings.json"`} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command %q does not contain %q", command, expected)
		}
	}
	if _, err := windowsValidationCommand(`C:\bad"path`, "service", `C:\result`, "", ""); err == nil {
		t.Fatal("validation command accepted a quoted path")
	}
	draft, err := windowsValidationCommand(`C:\Program Files\Airlock\connector.exe`, "airlock-validation", `C:\ProgramData\Airlock\result.json`, "", "")
	if err != nil || !strings.HasSuffix(draft, " --draft") {
		t.Fatalf("draft validation command = %q, %v", draft, err)
	}
}

func TestWindowsStopIsIdempotentForStoppedInstances(t *testing.T) {
	t.Run("scheduled task", func(t *testing.T) {
		operations := &scriptedWindowsOperations{run: func(string, ...string) ([]byte, error) {
			return []byte("ERROR: The task is not currently running."), errors.New("exit status 1")
		}}
		service := newServiceManager("sample", t.TempDir(), ServiceUser, operations, nil).(*windowsService)
		if err := service.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("system service", func(t *testing.T) {
		operations := &scriptedWindowsOperations{run: func(_ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "query" {
				return []byte("STATE : 1 STOPPED"), nil
			}
			return []byte("FAILED 1062"), errors.New("service has not been started")
		}}
		service := newServiceManager("sample", t.TempDir(), ServiceSystem, operations, nil).(*windowsService)
		if err := service.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("access denied", func(t *testing.T) {
		operations := &scriptedWindowsOperations{run: func(string, ...string) ([]byte, error) {
			return []byte("Access is denied"), errors.New("exit status 5")
		}}
		service := newServiceManager("sample", t.TempDir(), ServiceUser, operations, nil).(*windowsService)
		if err := service.Stop(context.Background()); err == nil {
			t.Fatal("Stop ignored an access-denied error")
		}
	})
}

func TestWindowsServiceRequestedStateTransitions(t *testing.T) {
	tests := []struct {
		state, wanted string
		allowed       bool
	}{
		{state: "START_PENDING", wanted: "RUNNING", allowed: true},
		{state: "STOPPED", wanted: "RUNNING"},
		{state: "START_PENDING", wanted: "STOPPED", allowed: true},
		{state: "RUNNING", wanted: "STOPPED", allowed: true},
		{state: "STOP_PENDING", wanted: "STOPPED", allowed: true},
		{state: "PAUSED", wanted: "STOPPED"},
	}
	for _, test := range tests {
		if got := windowsServiceCanTransition(test.state, test.wanted); got != test.allowed {
			t.Errorf("windowsServiceCanTransition(%q, %q) = %t, want %t", test.state, test.wanted, got, test.allowed)
		}
	}
	queries := 0
	operations := &scriptedWindowsOperations{run: func(_ string, args ...string) ([]byte, error) {
		queries++
		if queries == 1 {
			return []byte("STATE : 4 RUNNING"), nil
		}
		return []byte("STATE : 1 STOPPED"), nil
	}}
	service := newServiceManager("sample", t.TempDir(), ServiceSystem, operations, nil).(*windowsService)
	if err := service.waitNamedSC(context.Background(), "sample", "STOPPED"); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsRollbackAssumesCallerOwnsStop(t *testing.T) {
	stateDir := t.TempDir()
	operations := &scriptedWindowsOperations{run: func(string, ...string) ([]byte, error) { return nil, nil }}
	service := newServiceManager("sample", stateDir, ServiceUser, operations, nil).(*windowsService)
	if err := os.MkdirAll(filepath.Dir(service.binary()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.binary(), []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.binary()+".rollback", []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := service.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range operations.calls {
		if strings.Contains(call, " /End ") {
			t.Fatalf("Rollback stopped the scheduled task again: %v", operations.calls)
		}
	}
	if len(operations.calls) != 1 || !strings.Contains(operations.calls[0], " /Run ") {
		t.Fatalf("rollback calls = %v", operations.calls)
	}
	body, err := os.ReadFile(service.binary())
	if err != nil || string(body) != "old" {
		t.Fatalf("binary = %q, %v", body, err)
	}
}

func TestWindowsScheduledTaskCandidateExitManualAndAutomaticRollback(t *testing.T) {
	for _, path := range []string{"manual", "automatic"} {
		t.Run(path, func(t *testing.T) {
			stateDir := t.TempDir()
			operations := &scriptedWindowsOperations{run: func(_ string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "/End" {
					return []byte("ERROR: The task is not currently running."), errors.New("exit status 1")
				}
				return nil, nil
			}}
			service := newServiceManager("sample", stateDir, ServiceUser, operations, nil).(*windowsService)
			if err := os.MkdirAll(filepath.Dir(service.binary()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(service.binary(), []byte("candidate"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(service.binary()+".rollback", []byte("old"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := service.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := service.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(operations.calls, "\n")
			if strings.Count(joined, " /End ") != 1 || strings.Count(joined, " /Run ") != 1 {
				t.Fatalf("rollback calls = %v", operations.calls)
			}
		})
	}
}
