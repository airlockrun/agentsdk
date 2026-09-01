package protocol

import (
	"encoding/json"
	"math"
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
		Targets: []string{PlatformLinuxAMD64}, ArtifactDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
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
}

func TestValidateManifestInt32Bounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest, int)
	}{
		{name: "protocol minor", mutate: func(manifest *Manifest, value int) { manifest.ProtocolMinor = value }},
		{name: "command revision", mutate: func(manifest *Manifest, value int) { manifest.Interface.Commands[0].Revision = value }},
		{name: "directory revision", mutate: func(manifest *Manifest, value int) { manifest.Interface.Directories[0].Revision = value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validProtocolTestManifest(t)
			schema := json.RawMessage(`{"type":"object"}`)
			hash, err := HashJSON(schema)
			if err != nil {
				t.Fatal(err)
			}
			manifest.Interface.Commands = []CommandDescriptor{{Name: "ping", Revision: 1, Mode: CommandModeUnary, InputSchema: schema, OutputSchema: schema, InputSchemaHash: hash, OutputSchemaHash: hash}}
			manifest.Interface.Directories = []DirectoryDescriptor{{Name: "files", Revision: 1, Read: true}}
			for _, boundary := range []struct {
				value int
				ok    bool
			}{{value: math.MaxInt32, ok: true}, {value: math.MaxInt32 + 1}} {
				test.mutate(&manifest, boundary.value)
				manifest.InterfaceHash, err = InterfaceDigest(manifest.Interface)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateManifest(manifest); (err == nil) != boundary.ok {
					t.Fatalf("value %d: error = %v, ok = %t", boundary.value, err, boundary.ok)
				}
			}
		})
	}
}

func TestValidateManifestTargets(t *testing.T) {
	targets := []string{
		PlatformLinuxAMD64, PlatformLinuxARM64, PlatformLinuxARMv7,
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
}

func validProtocolTestManifest(t *testing.T) Manifest {
	t.Helper()
	manifest := Manifest{
		ProtocolMajor: Major, ProtocolMinor: Minor, Targets: []string{PlatformLinuxAMD64},
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
	for name, encoded := range map[string][]byte{"manifest": manifestJSON} {
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
