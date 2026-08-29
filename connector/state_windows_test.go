//go:build windows

package connector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type windowsTestOperations struct{ calls []string }

func (o *windowsTestOperations) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	o.calls = append(o.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}
func (*windowsTestOperations) Executable() (string, error) { return `C:\connector.exe`, nil }

func TestWindowsSystemStateUsesProgramDataAndACL(t *testing.T) {
	programData := t.TempDir()
	t.Setenv("ProgramData", programData)
	path, err := defaultStateDir("sample", ServiceSystem)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(programData, "Airlock", "Connectors", "sample") {
		t.Fatalf("path = %s", path)
	}
	operations := &windowsTestOperations{}
	if err := prepareStateDirectory(path, ServiceSystem, operations); err != nil {
		t.Fatal(err)
	}
	if len(operations.calls) != 1 || strings.Contains(operations.calls[0], `LOCAL SERVICE`) {
		t.Fatalf("calls = %#v", operations.calls)
	}
	installationID := "00000000-0000-0000-0000-000000000001"
	stateDir := filepath.Join(path, "installations", installationID)
	service := newServiceManager("sample", stateDir, ServiceSystem, operations, nil).(*windowsService)
	if err := service.PrepareIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(operations.calls) != 2 || !strings.Contains(operations.calls[1], service.account()+`:(OI)(CI)F`) {
		t.Fatalf("calls = %#v", operations.calls)
	}
}

func TestWindowsMachineDPAPIRoundTrip(t *testing.T) {
	protected, err := protectBytes([]byte("secret"), true)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := unprotectBytes(protected, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "secret" {
		t.Fatalf("plain = %q", plain)
	}
}
