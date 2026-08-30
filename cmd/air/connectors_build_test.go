package main

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

func TestDiscoverConnectors(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zeta", "alpha-one"} {
		if err := os.MkdirAll(filepath.Join(root, "connectors", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := discoverConnectors(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].slug != "alpha-one" || targets[1].slug != "zeta" {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestConnectorBuildTarget(t *testing.T) {
	tests := []struct {
		target, goos, goarch, suffix string
		environment                  []string
	}{
		{protocol.PlatformLinuxAMD64, "linux", "amd64", "", []string{"GOOS=linux", "GOARCH=amd64"}},
		{protocol.PlatformLinuxARM64, "linux", "arm64", "", []string{"GOOS=linux", "GOARCH=arm64"}},
		{protocol.PlatformLinuxARMv7, "linux", "arm", "", []string{"GOOS=linux", "GOARCH=arm", "GOARM=7"}},
		{protocol.PlatformDarwinAMD64, "darwin", "amd64", "", []string{"GOOS=darwin", "GOARCH=amd64"}},
		{protocol.PlatformDarwinARM64, "darwin", "arm64", "", []string{"GOOS=darwin", "GOARCH=arm64"}},
		{protocol.PlatformWindowsAMD64, "windows", "amd64", ".exe", []string{"GOOS=windows", "GOARCH=amd64"}},
		{protocol.PlatformWindowsARM64, "windows", "arm64", ".exe", []string{"GOOS=windows", "GOARCH=arm64"}},
	}
	for _, test := range tests {
		t.Run(test.target, func(t *testing.T) {
			target, err := connectorBuildTarget(test.target)
			if err != nil || target.GOOS != test.goos || target.GOARCH != test.goarch || target.ExecutableSuffix != test.suffix || !reflect.DeepEqual(target.GoEnv(), test.environment) {
				t.Fatalf("connectorBuildTarget(%q) = %+v, %v", test.target, target, err)
			}
		})
	}
	if _, err := connectorBuildTarget("plan9-amd64"); err == nil {
		t.Fatal("connectorBuildTarget accepted an unsupported target")
	}
}

func TestInspectConnectorManifestBoundsDescendantPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "manifest-descendant")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 30 &\nprintf '{}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := inspectConnectorManifest(dir, binary)
	if err == nil {
		t.Fatal("manifest inspection waited for an inherited descendant pipe")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("manifest descendant cleanup took %s", elapsed)
	}
}

func TestDiscoverConnectorsRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "connectors", "README.md"), []byte("not a target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverConnectors(root); err == nil {
		t.Fatal("discoverConnectors accepted a non-directory child")
	}
}

func TestDiscoverConnectorsRejectsSymlinks(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "connectors", "linked")); err != nil {
		t.Skip(err)
	}
	if _, err := discoverConnectors(root); err == nil {
		t.Fatal("symlink connector was accepted")
	}
}

func TestInspectConnectorManifestBoundsStreams(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "manifest")
	script := "#!/bin/sh\nprintf '%*s' " + strconv.Itoa(protocol.MaxManifestBytes+1) + " ''\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectConnectorManifest(dir, binary); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}
