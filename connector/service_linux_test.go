//go:build linux

package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeOperations struct {
	executable string
	calls      []string
}

func (f *fakeOperations) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return []byte("active\n"), nil
}
func (f *fakeOperations) Executable() (string, error) { return f.executable, nil }

type lifecycleOperations struct {
	executable string
	running    bool
}

func (o *lifecycleOperations) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "systemctl" || len(args) == 0 {
		return nil, nil
	}
	for _, arg := range args {
		switch arg {
		case "start":
			o.running = true
		case "stop":
			o.running = false
		case "is-active":
			if o.running {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), nil
		}
	}
	return nil, nil
}

func (o *lifecycleOperations) Executable() (string, error) { return o.executable, nil }

func TestLinuxUserServiceLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	operations := &fakeOperations{executable: executable}
	manager := newServiceManager("sample", filepath.Join(home, "state"), ServiceUser, operations, nil)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(operations.calls, "\n")
	for _, expected := range []string{"systemctl --user daemon-reload", "systemctl --user enable", "systemctl --user start", "systemctl --user is-active"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("calls %q do not contain %q", joined, expected)
		}
	}
}

func TestSystemdUnitAllowsDeclaredWritableDirectoryRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "state")
	writableRoot := filepath.Join(t.TempDir(), "managed files")
	manager := newServiceManager("sample", stateDir, ServiceUser, &fakeOperations{executable: executable}, []string{writableRoot}).(*linuxService)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(manager.unitPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stateDir, writableRoot} {
		quoted, _ := systemdQuote(path)
		if !strings.Contains(string(unit), quoted) {
			t.Fatalf("unit does not allow writable root %q: %s", path, unit)
		}
	}
}

func TestSystemdReconfigureIsTransactional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "state")
	oldRoot, newRoot := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	operations := &fakeOperations{executable: executable}
	oldManager := newServiceManager("sample", stateDir, ServiceUser, operations, []string{oldRoot}).(*linuxService)
	if err := oldManager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	newManager := newServiceManager("sample", stateDir, ServiceUser, operations, []string{newRoot}).(*linuxService)
	restore, err := newManager.Reconfigure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertUnitRoot(t, newManager.unitPath(), newRoot, oldRoot)
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	assertUnitRoot(t, oldManager.unitPath(), oldRoot, newRoot)
}

func TestSystemdUpgradeAndRollbackRegenerateWritableRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable := filepath.Join(t.TempDir(), "connector")
	if err := os.WriteFile(executable, []byte("old binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(home, "state")
	oldRoot, newRoot := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	operations := &lifecycleOperations{executable: executable}
	manager := newServiceManager("sample", stateDir, ServiceUser, operations, []string{oldRoot}).(*linuxService)
	if err := manager.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("new binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager.writableRoots = []string{newRoot}
	if ready, err := manager.Upgrade(context.Background(), func() error { return nil }); err != nil || !ready {
		t.Fatalf("Upgrade() = %t, %v", ready, err)
	}
	assertUnitRoot(t, manager.unitPath(), newRoot, oldRoot)
	operations.running = false
	if err := manager.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertUnitRoot(t, manager.unitPath(), oldRoot, newRoot)
}

func assertUnitRoot(t *testing.T, unitPath, want, unwanted string) {
	t.Helper()
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	wantQuoted, _ := systemdQuote(want)
	unwantedQuoted, _ := systemdQuote(unwanted)
	if !strings.Contains(string(body), wantQuoted) || strings.Contains(string(body), unwantedQuoted) {
		t.Fatalf("unit roots = %s, want %s without %s", body, wantQuoted, unwantedQuoted)
	}
}

func TestSystemdQuoteEscapesPathsAndSpecifiers(t *testing.T) {
	quoted, err := systemdQuote(`/var/lib/airlock/a path/%i/"quoted"`)
	if err != nil {
		t.Fatal(err)
	}
	if quoted != `"/var/lib/airlock/a path/%%i/\"quoted\""` {
		t.Fatalf("quoted = %q", quoted)
	}
	for _, value := range []string{"relative", "/bad\npath"} {
		if _, err := systemdQuote(value); err == nil {
			t.Fatalf("systemdQuote(%q) succeeded", value)
		}
	}
}

func TestLinuxSystemServiceBinaryIsOutsideWritableState(t *testing.T) {
	stateDir := filepath.Join("/var/lib/airlock/connectors/sample/installations", "00000000-0000-0000-0000-000000000001")
	service := newServiceManager("sample", stateDir, ServiceSystem, &fakeOperations{}, nil).(*linuxService)
	if strings.HasPrefix(service.binary(), stateDir+string(filepath.Separator)) {
		t.Fatalf("system service binary %q is beneath writable state %q", service.binary(), stateDir)
	}
	if service.binary() != "/usr/local/lib/airlock-connectors/airlock-connector-sample-00000000-0000-0000-0000-000000000001" {
		t.Fatalf("binary = %q", service.binary())
	}
}

func TestLinuxSystemValidationUsesDedicatedServiceIdentity(t *testing.T) {
	installationID := "00000000-0000-0000-0000-000000000001"
	stateDir := filepath.Join(t.TempDir(), "installations", installationID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	operations := &fakeOperations{executable: executable}
	service := newServiceManager("sample", stateDir, ServiceSystem, operations, nil).(*linuxService)
	stagedSettings := filepath.Join(stateDir, ".upgrade-settings.json")
	if err := service.ValidateIdentity(context.Background(), installationID, stagedSettings); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(operations.calls, "\n")
	account := service.account()
	for _, expected := range []string{
		"chown -R " + account + ":" + account + " " + stateDir,
		"/usr/bin/env -u AIRLOCK_CONNECTOR_INSTALLATION_ID -u AIRLOCK_CONNECTOR_MODE /usr/sbin/runuser -u " + account + " -- " + executable + " validate-service --identity " + account + " --installation " + installationID + " --settings-file " + stagedSettings,
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("calls %q do not contain %q", joined, expected)
		}
	}
	other := newServiceManager("sample", filepath.Join(filepath.Dir(stateDir), "00000000-0000-0000-0000-000000000002"), ServiceSystem, operations, nil).(*linuxService)
	if service.account() == other.account() {
		t.Fatalf("service identities are shared across installations: %q", service.account())
	}
	if len(service.account()) > 31 || len(other.account()) > 31 {
		t.Fatalf("service identities exceed Linux account limit: %q and %q", service.account(), other.account())
	}
}

func TestSystemConfigurationDelegatesSelfTestToServiceIdentity(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state", "sample")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	operations := &fakeOperations{executable: executable}
	selfTests := 0
	runtime := New(Config{
		Kind: "sample", Contract: DefineContract("io.airlockrun.system_validation"), Name: "Sample", Description: "System validation.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceSystem, StateDirectory: stateDir, Operations: operations,
		Input: strings.NewReader(""), Output: &strings.Builder{}, ErrorOutput: &strings.Builder{}, SelfTest: func(context.Context) error { selfTests++; return nil },
	})
	if err := runtime.RunContext(context.Background(), []string{"configure", "--non-interactive"}); err != nil {
		t.Fatal(err)
	}
	if selfTests != 0 {
		t.Fatalf("self-test ran %d times as the configuring identity", selfTests)
	}
	draftDir := draftStateDirectory(stateDir)
	manager := newServiceManager("sample", draftDir, ServiceSystem, operations, nil).(*linuxService)
	if !strings.Contains(strings.Join(operations.calls, "\n"), "validate-service --identity "+manager.account()+" --draft") {
		t.Fatalf("calls = %v", operations.calls)
	}
	joined := strings.Join(operations.calls, "\n")
	if !strings.Contains(joined, "chown -R "+manager.account()+":"+manager.account()+" "+draftDir) || strings.Contains(joined, "chown -R "+manager.account()+":"+manager.account()+" "+stateDir+"\n") {
		t.Fatalf("draft validation touched shared state parent: %v", operations.calls)
	}
	state, err := loadInstallation(filepath.Join(draftDir, "installation.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	if state.ServiceMode != ServiceSystem {
		t.Fatalf("service mode = %q", state.ServiceMode)
	}
}
