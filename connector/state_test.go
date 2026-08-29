package connector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMultipleInstallationsRequireSelection(t *testing.T) {
	base := t.TempDir()
	ids := []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"}
	for _, id := range ids {
		dir := filepath.Join(base, "installations", id)
		if err := saveInstallation(filepath.Join(dir, "installation.json"), installationState{Version: 1, ServiceMode: ServiceUser, InstallationID: id, Credential: "credential", Enabled: true}, false); err != nil {
			t.Fatal(err)
		}
	}
	output := &bytes.Buffer{}
	userRuntime := New(Config{Kind: "multi", Contract: DefineContract("io.airlockrun.multi_test"), Name: "Multi", Description: "Multiple installations.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser, StateDirectory: base, Output: output, ErrorOutput: output, Input: bytes.NewBuffer(nil)})
	if err := userRuntime.RunContext(context.Background(), []string{"version"}); err == nil {
		t.Fatal("ambiguous installation was accepted")
	}
	if err := userRuntime.RunContext(context.Background(), []string{"version", "--installation", ids[0]}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		return
	}
	systemRuntime := New(Config{Kind: "multi", Contract: DefineContract("io.airlockrun.multi_test"), Name: "Multi", Description: "Multiple installations.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceSystem, StateDirectory: base, Output: output, ErrorOutput: output, Input: bytes.NewBuffer(nil)})
	if err := systemRuntime.RunContext(context.Background(), []string{"version", "--installation", ids[0]}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("system mode selected user installation: %v", err)
	}
}

func TestAtomicWritePreservesExistingDirectoryMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix directory permission bits")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(dir, "unit.service"), []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("directory mode = %o, want 755", info.Mode().Perm())
	}
}

func TestInstallationCommandsUseProcessLock(t *testing.T) {
	base := t.TempDir()
	lock, err := acquireFileLock(context.Background(), filepath.Join(draftStateDirectory(base), ".installation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	runtime := New(Config{Kind: "locked", Contract: DefineContract("io.airlockrun.locked_test"), Name: "Locked", Description: "Lock test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: &struct{}{}, ServiceMode: ServiceUser, StateDirectory: base, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = runtime.RunContext(ctx, []string{"configure", "--non-interactive"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configure error = %v", err)
	}
}

func TestInstallationLocksAreScopedPerInstallation(t *testing.T) {
	base := t.TempDir()
	ids := []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002"}
	for _, id := range ids {
		stateDir := filepath.Join(base, "installations", id)
		if err := saveInstallation(filepath.Join(stateDir, "installation.json"), installationState{ServiceMode: ServiceUser, InstallationID: id, Credential: strings.Repeat("c", 32), Enabled: true}, false); err != nil {
			t.Fatal(err)
		}
		if err := saveSettingsSchema(filepath.Join(stateDir, "settings-schema.json"), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := acquireFileLock(context.Background(), filepath.Join(base, "installations", ids[0], ".installation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	runtime := New(Config{Kind: "scoped", Contract: DefineContract("io.airlockrun.scoped_test"), Name: "Scoped", Description: "Scoped lock test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: &struct{}{}, ServiceMode: ServiceUser, StateDirectory: base, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}})
	if err := runtime.RunContext(context.Background(), []string{"configure", "--installation", ids[1], "--non-interactive"}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationDirectoryRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	root := filepath.Join(base, "installations")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "00000000-0000-0000-0000-000000000001")); err != nil {
		t.Skip(err)
	}
	if _, err := installationDirectories(base); err == nil {
		t.Fatal("symlink installation was accepted")
	}
}

func TestServiceModeIsExplicitAndPersisted(t *testing.T) {
	stateDir := t.TempDir()
	userRuntime := New(Config{Kind: "test", Contract: DefineContract("io.airlockrun.connector_test"), Name: "Test", Description: "Test connector.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: &struct{}{}, ServiceMode: ServiceUser, StateDirectory: stateDir, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}})
	if err := userRuntime.RunContext(context.Background(), []string{"configure", "--non-interactive"}); err != nil {
		t.Fatal(err)
	}
	state, err := loadInstallation(filepath.Join(draftStateDirectory(stateDir), "installation.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if state.ServiceMode != ServiceUser {
		t.Fatalf("service mode = %q", state.ServiceMode)
	}

	wantModeError := "does not match"
	if runtime.GOOS == "darwin" {
		wantModeError = "macOS system services are unsupported"
	}
	assertPanicContains(t, wantModeError, func() {
		New(Config{Kind: "test", Contract: DefineContract("io.airlockrun.connector_test"), Name: "Test", Description: "Test connector.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceSystem, StateDirectory: stateDir})
	})
	assertPanicContains(t, "must explicitly select", func() {
		New(Config{Kind: "test", Contract: DefineContract("io.airlockrun.connector_test"), Name: "Test", Description: "Test connector.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, StateDirectory: t.TempDir()})
	})
}

func assertPanicContains(t *testing.T, want string, run func()) {
	t.Helper()
	defer func() {
		value := recover()
		if value == nil || !strings.Contains(value.(string), want) {
			t.Fatalf("panic = %v, want substring %q", value, want)
		}
	}()
	run()
}
