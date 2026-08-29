//go:build darwin

package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type darwinFakeOperations struct {
	executable    string
	registered    bool
	running       bool
	bootstrapRuns bool
	calls         []string
}

func (f *darwinFakeOperations) Executable() (string, error) { return f.executable, nil }

func (f *darwinFakeOperations) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "/bin/launchctl" || len(args) == 0 {
		return nil, nil
	}
	switch args[0] {
	case "print":
		if !f.registered {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		if f.running {
			return []byte("service = {\n\t state\t =   running \n}\n"), nil
		}
		return []byte("service = {\n\tstate = waiting\n\tlast state = running\n}\n"), nil
	case "bootstrap":
		f.registered, f.running = true, f.bootstrapRuns
		return nil, nil
	case "kickstart":
		if !f.registered {
			return nil, errors.New("service is not registered")
		}
		f.running = true
		return nil, nil
	case "bootout":
		if !f.registered {
			return []byte("Could not find service"), errors.New("exit status 113")
		}
		f.registered, f.running = false, false
		return nil, nil
	case "enable", "disable":
		return nil, nil
	default:
		return nil, errors.New("unexpected launchctl operation")
	}
}

func newDarwinTestService(t *testing.T) (*darwinService, *darwinFakeOperations, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(executable, []byte("old binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-0000-0000-000000000001"
	stateDir := filepath.Join(home, "Library", "Application Support", "Airlock", "Connectors", "sample", "installations", id)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	operations := &darwinFakeOperations{executable: executable}
	service := newServiceManager("sample", stateDir, ServiceUser, operations, nil).(*darwinService)
	return service, operations, executable
}

func TestDarwinPlistEscapesAndPinsRuntimeEnvironment(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	body, err := launchdPlist(launchdPlistConfig{
		label: "run.airlock.connector.sample", binary: "/tmp/a&b/connector", stateDir: "/tmp/a&b", installationID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{"a&amp;b", "AIRLOCK_CONNECTOR_INSTALLATION_ID", id, "AIRLOCK_CONNECTOR_MODE", "<string></string>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plist does not contain %q: %s", expected, text)
		}
	}
	if err := validateLaunchdPlist(body); err != nil {
		t.Fatal(err)
	}
	if _, err := launchdPlist(launchdPlistConfig{label: "sample", binary: "relative", stateDir: "/tmp", installationID: id}); err == nil {
		t.Fatal("launchdPlist accepted a relative executable")
	}
}

func TestDarwinPlistSemanticValidation(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	body, err := launchdPlist(launchdPlistConfig{
		label: "run.airlock.connector.sample", binary: "/tmp/connector", stateDir: "/tmp/state", installationID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := string(body)
	tests := []struct {
		name, body string
	}{
		{name: "root is not dict", body: `<?xml version="1.0"?><plist version="1.0"><array></array></plist>`},
		{name: "duplicate label", body: strings.Replace(valid, "<key>ProgramArguments</key>", "<key>Label</key><string>duplicate</string><key>ProgramArguments</key>", 1)},
		{name: "invalid label", body: strings.Replace(valid, "run.airlock.connector.sample", "sample", 1)},
		{name: "relative executable", body: strings.Replace(valid, "<string>/tmp/connector</string>", "<string>relative</string>", 1)},
		{name: "wrong command", body: strings.Replace(valid, "<string>run</string>", "<string>status</string>", 1)},
		{name: "missing required path", body: strings.Replace(valid, "WorkingDirectory", "OtherDirectory", 1)},
		{name: "duplicate environment", body: strings.Replace(valid, "AIRLOCK_CONNECTOR_INSTALLATION_ID", "AIRLOCK_CONNECTOR_MODE", 1)},
		{name: "manifest mode inherited", body: strings.Replace(valid, "<string></string>", "<string>manifest</string>", 1)},
		{name: "false lifecycle boolean", body: strings.Replace(valid, "<true></true>", "<false></false>", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateLaunchdPlist([]byte(test.body)); err == nil {
				t.Fatal("validateLaunchdPlist accepted an invalid service definition")
			}
		})
	}
}

func TestDarwinUserLifecycleUsesActualRunningState(t *testing.T) {
	service, operations, _ := newDarwinTestService(t)
	operations.registered = true
	if status, err := service.Status(context.Background()); err != nil || status != "inactive" {
		t.Fatalf("stale Status() = %q, %v", status, err)
	}
	if err := service.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if operations.registered {
		t.Fatal("Install left a stale LaunchAgent registered")
	}
	info, err := os.Stat(service.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("LaunchAgent mode = %o", info.Mode().Perm())
	}
	if err := service.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "use upgrade") {
		t.Fatalf("second Install() error = %v", err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := service.Status(context.Background()); err != nil || status != "active" {
		t.Fatalf("running Status() = %q, %v", status, err)
	}
	before := len(operations.calls)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(operations.calls) != before+1 || !strings.Contains(operations.calls[len(operations.calls)-1], "launchctl print") {
		t.Fatalf("idempotent Start calls = %v", operations.calls[before:])
	}
	if err := service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() = %v", err)
	}
	joined := strings.Join(operations.calls, "\n")
	for _, expected := range []string{
		"/bin/launchctl bootstrap gui/" + strconv.Itoa(os.Getuid()),
		"/bin/launchctl kickstart gui/" + strconv.Itoa(os.Getuid()),
		"/bin/launchctl bootout gui/" + strconv.Itoa(os.Getuid()),
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("calls do not contain %q:\n%s", expected, joined)
		}
	}
}

func TestDarwinEnableDisableAndIdempotentUninstall(t *testing.T) {
	service, operations, _ := newDarwinTestService(t)
	if err := service.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if operations.registered || operations.running {
		t.Fatal("Disable left the LaunchAgent active")
	}
	if err := service.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Uninstall(context.Background()); err != nil {
		t.Fatalf("second Uninstall() = %v", err)
	}
}

func TestDarwinReconfigureIsTransactional(t *testing.T) {
	service, _, _ := newDarwinTestService(t)
	if err := service.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	previous, err := launchdPlist(launchdPlistConfig{
		label: service.label(), binary: "/tmp/retained-connector", stateDir: service.stateDir, installationID: service.installationID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(service.plistPath(), previous, 0o600); err != nil {
		t.Fatal(err)
	}
	restore, err := service.Reconfigure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configured, err := os.ReadFile(service.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(configured) == string(previous) {
		t.Fatal("Reconfigure did not install the current plist")
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(service.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(previous) {
		t.Fatal("Reconfigure restore did not recover the retained plist")
	}
}

func TestDarwinUpgradeRollbackAndRollbackSetSafety(t *testing.T) {
	service, _, executable := newDarwinTestService(t)
	ctx := context.Background()
	if err := service.Install(ctx); err != nil {
		t.Fatal(err)
	}
	oldPlist, err := os.ReadFile(service.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	setDarwinRollbackGeneration(t, service, "generation-one")
	if committed, err := service.retainRollbackSet([]byte("first retained"), oldPlist); err != nil || !committed {
		t.Fatal(err)
	}
	if _, err := service.retainRollbackSet([]byte("corrupt replacement"), []byte("not XML")); err == nil {
		t.Fatal("retainRollbackSet accepted an invalid plist")
	}
	retained, _, err := service.readRollbackSet()
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != "first retained" {
		t.Fatalf("retained binary = %q", retained)
	}
	if err := os.WriteFile(executable, []byte("new binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	ready, err := service.Upgrade(ctx, func() error { return nil })
	if err != nil || !ready {
		t.Fatalf("Upgrade() = %t, %v", ready, err)
	}
	assertDarwinFile(t, service.binary(), "new binary")
	if err := service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertDarwinFile(t, service.binary(), "old binary")
}

func TestDarwinRollbackPointerFailurePreservesGenerationBinding(t *testing.T) {
	service, _, _ := newDarwinTestService(t)
	if err := service.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(service.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	setDarwinRollbackGeneration(t, service, "generation-one")
	if committed, err := service.retainRollbackSet([]byte("binary-one"), plist); err != nil || !committed {
		t.Fatalf("initial retain = %t, %v", committed, err)
	}

	setDarwinRollbackGeneration(t, service, "generation-two")
	service.rollbackHook = func(phase string) error {
		if phase == "before-pointer" {
			return errors.New("injected before pointer switch")
		}
		return nil
	}
	committed, err := service.retainRollbackSet([]byte("binary-two"), plist)
	if err == nil || committed {
		t.Fatalf("before-pointer retain = %t, %v", committed, err)
	}
	if _, _, err := service.readRollbackSet(); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("mismatched rollback read error = %v", err)
	}
	if _, err := service.RollbackDigest(); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("mismatched RollbackDigest error = %v", err)
	}
	if err := service.Rollback(context.Background()); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("mismatched Rollback error = %v", err)
	}
	setDarwinRollbackGeneration(t, service, "generation-one")
	binary, _, err := service.readRollbackSet()
	if err != nil || string(binary) != "binary-one" {
		t.Fatalf("restored prior set = %q, %v", binary, err)
	}

	setDarwinRollbackGeneration(t, service, "generation-two")
	service.rollbackHook = func(phase string) error {
		if phase == "after-pointer" {
			return errors.New("injected after pointer switch")
		}
		return nil
	}
	committed, err = service.retainRollbackSet([]byte("binary-two"), plist)
	if err == nil || !committed {
		t.Fatalf("after-pointer retain = %t, %v", committed, err)
	}
	binary, _, err = service.readRollbackSet()
	if err != nil || string(binary) != "binary-two" {
		t.Fatalf("committed new set = %q, %v", binary, err)
	}
	if _, err := service.RollbackDigest(); err != nil {
		t.Fatal(err)
	}
}

func setDarwinRollbackGeneration(t *testing.T, service *darwinService, generation string) {
	t.Helper()
	if err := atomicWrite(filepath.Join(service.stateDir, "rollback-state.json"), []byte(generation), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertDarwinFile(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}

func TestDarwinSystemServiceFailsLoud(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	service := newServiceManager("sample", filepath.Join(home, "state"), ServiceSystem, &darwinFakeOperations{}, nil)
	if err := service.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Install(system) error = %v", err)
	}
}
