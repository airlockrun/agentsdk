package protocol

import (
	"reflect"
	"testing"
)

func TestSupportedTargets(t *testing.T) {
	tests := []struct {
		id, architecture, goos, goarch, suffix string
		environment                            []string
		user, system                           bool
	}{
		{id: PlatformLinuxAMD64, architecture: "amd64", goos: "linux", goarch: "amd64", environment: []string{"GOOS=linux", "GOARCH=amd64"}, user: true, system: true},
		{id: PlatformLinuxARM64, architecture: "arm64", goos: "linux", goarch: "arm64", environment: []string{"GOOS=linux", "GOARCH=arm64"}, user: true, system: true},
		{id: PlatformLinuxARMv7, architecture: "armv7", goos: "linux", goarch: "arm", environment: []string{"GOOS=linux", "GOARCH=arm", "GOARM=7"}, user: true, system: true},
		{id: PlatformDarwinAMD64, architecture: "amd64", goos: "darwin", goarch: "amd64", environment: []string{"GOOS=darwin", "GOARCH=amd64"}, user: true},
		{id: PlatformDarwinARM64, architecture: "arm64", goos: "darwin", goarch: "arm64", environment: []string{"GOOS=darwin", "GOARCH=arm64"}, user: true},
		{id: PlatformWindowsAMD64, architecture: "amd64", goos: "windows", goarch: "amd64", suffix: ".exe", environment: []string{"GOOS=windows", "GOARCH=amd64"}, user: true, system: true},
		{id: PlatformWindowsARM64, architecture: "arm64", goos: "windows", goarch: "arm64", suffix: ".exe", environment: []string{"GOOS=windows", "GOARCH=arm64"}, user: true, system: true},
	}
	targets := SupportedTargets()
	if len(targets) != len(tests) {
		t.Fatalf("SupportedTargets length = %d, want %d", len(targets), len(tests))
	}
	seen := make(map[string]bool, len(targets))
	for i, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			target, ok := LookupTarget(test.id)
			if !ok {
				t.Fatal("LookupTarget did not find target")
			}
			if target.ID != test.id || target.Architecture != test.architecture || target.GOOS != test.goos || target.GOARCH != test.goarch || target.ExecutableSuffix != test.suffix {
				t.Fatalf("target = %+v", target)
			}
			if !reflect.DeepEqual(target.GoEnv(), test.environment) {
				t.Fatalf("GoEnv() = %v, want %v", target.GoEnv(), test.environment)
			}
			if target.SupportsServiceMode("user") != test.user || target.SupportsServiceMode("system") != test.system {
				t.Fatalf("service modes user=%t system=%t", target.SupportsServiceMode("user"), target.SupportsServiceMode("system"))
			}
			if targets[i].ID != test.id || seen[test.id] {
				t.Fatalf("target order or uniqueness failed at %q", test.id)
			}
			seen[test.id] = true
		})
	}
	if _, ok := LookupTarget("freebsd-amd64"); ok {
		t.Fatal("LookupTarget accepted an unsupported target")
	}
}

func TestSupportedTargetsDoNotExposeRegistryState(t *testing.T) {
	targets := SupportedTargets()
	targets[0].ID = "changed"
	environment := targets[1].GoEnv()
	environment[0] = "GOOS=changed"

	target, ok := LookupTarget(PlatformLinuxAMD64)
	if !ok || target.ID != PlatformLinuxAMD64 {
		t.Fatalf("registry target changed: %+v", target)
	}
	if got := SupportedTargets()[1].GoEnv()[0]; got != "GOOS=linux" {
		t.Fatalf("registry environment changed: %q", got)
	}
}
