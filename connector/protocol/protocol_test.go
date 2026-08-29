package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	values := []string{`{"b":2,"a":1}`, "{\n \"a\": 1, \"b\": 2\n}"}
	var hash string
	for _, value := range values {
		got, err := HashJSON([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if hash != "" && got != hash {
			t.Fatalf("HashJSON(%s) = %s, want %s", value, got, hash)
		}
		hash = got
	}
}

func TestValidateContractID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{name: "reverse domain", id: "io.airlockrun.transmission", ok: true},
		{name: "agent namespace", id: "com.example.media_server.transmission", ok: true},
		{name: "missing namespace", id: "transmission"},
		{name: "uppercase", id: "IO.airlockrun.transmission"},
		{name: "empty segment", id: "io..transmission"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateContractID(test.id)
			if (err == nil) != test.ok {
				t.Fatalf("ValidateContractID(%q) error = %v, ok = %t", test.id, err, test.ok)
			}
		})
	}
}

func TestValidateManifest(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	hash, err := HashJSON(schema)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		ProtocolMajor: Major, ProtocolMinor: Minor,
		Targets: []string{PlatformLinuxAMD64}, ServiceMode: "user", ArtifactDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Interface: Interface{Kind: "sample", ContractID: "io.airlockrun.sample", Name: "Sample", Description: "Sample connector.", ArtifactVersion: "1",
			Commands: []CommandDescriptor{{Name: "ping", Revision: 1, Mode: CommandModeUnary, InputSchema: schema, OutputSchema: schema, InputSchemaHash: hash, OutputSchemaHash: hash}}},
	}
	manifest.InterfaceHash, err = InterfaceDigest(manifest.Interface)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Interface.Commands[0].Revision = 0
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("ValidateManifest accepted revision zero")
	}
	manifest.Interface.Commands[0].Revision = 1
	manifest.ServiceMode = ""
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("ValidateManifest accepted an empty service mode")
	}
}

func TestValidateManifestTargets(t *testing.T) {
	targets := []string{
		PlatformLinuxAMD64, PlatformLinuxARM64,
		PlatformDarwinAMD64, PlatformDarwinARM64,
		PlatformWindowsAMD64, PlatformWindowsARM64,
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			manifest := validProtocolTestManifest(t)
			manifest.Targets = []string{target}
			if err := ValidateManifest(manifest); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, targets := range [][]string{{"freebsd-amd64"}, {PlatformDarwinARM64, PlatformDarwinARM64}} {
		manifest := validProtocolTestManifest(t)
		manifest.Targets = targets
		if err := ValidateManifest(manifest); err == nil {
			t.Fatalf("ValidateManifest accepted targets %v", targets)
		}
	}
	manifest := validProtocolTestManifest(t)
	manifest.ServiceMode = "system"
	manifest.Targets = []string{PlatformLinuxAMD64, PlatformDarwinARM64}
	if err := ValidateManifest(manifest); err == nil || !strings.Contains(err.Error(), "only user service mode") {
		t.Fatalf("ValidateManifest(system Darwin) error = %v", err)
	}
}

func validProtocolTestManifest(t *testing.T) Manifest {
	t.Helper()
	manifest := Manifest{
		ProtocolMajor: Major, ProtocolMinor: Minor, Targets: []string{PlatformLinuxAMD64}, ServiceMode: "user",
		ArtifactDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Interface:      Interface{Kind: "sample", ContractID: "io.airlockrun.sample", Name: "Sample", Description: "Sample connector.", ArtifactVersion: "1"},
	}
	var err error
	manifest.InterfaceHash, err = InterfaceDigest(manifest.Interface)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestValidateArtifactDigest(t *testing.T) {
	for _, test := range []struct {
		name, value string
		ok          bool
	}{
		{name: "lowercase", value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ok: true},
		{name: "missing"},
		{name: "uppercase", value: "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"},
		{name: "short", value: "abcd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateArtifactDigest(test.value); (err == nil) != test.ok {
				t.Fatalf("ValidateArtifactDigest(%q) = %v", test.value, err)
			}
		})
	}
}

func TestArtifactDigestWireFields(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	manifestJSON, err := json.Marshal(Manifest{ArtifactDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	heartbeatJSON, err := json.Marshal(HeartbeatRequest{ArtifactDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{"manifest": manifestJSON, "heartbeat": heartbeatJSON} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := json.Unmarshal(fields["artifactDigest"], &got); err != nil || got != digest {
			t.Fatalf("%s artifactDigest = %q, %v (%s)", name, got, err, encoded)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	tests := []struct {
		name    string
		setting SettingDescriptor
		ok      bool
	}{
		{name: "duration", setting: SettingDescriptor{Name: "timeout", Kind: "duration", Default: "5s"}, ok: true},
		{name: "enum", setting: SettingDescriptor{Name: "mode", Kind: "enum", Enum: []string{"one", "two"}, Default: "two"}, ok: true},
		{name: "secret default", setting: SettingDescriptor{Name: "password", Kind: "secret", Default: "bad"}},
		{name: "reserved", setting: SettingDescriptor{Name: "check", Kind: "string"}},
		{name: "internal new flag", setting: SettingDescriptor{Name: "new", Kind: "string"}},
		{name: "internal settings file flag", setting: SettingDescriptor{Name: "settings-file", Kind: "string"}},
		{name: "bad integer", setting: SettingDescriptor{Name: "workers", Kind: "integer", Default: "many"}},
		{name: "enum default", setting: SettingDescriptor{Name: "mode", Kind: "enum", Enum: []string{"one"}, Default: "two"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSettings([]SettingDescriptor{test.setting})
			if (err == nil) != test.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
