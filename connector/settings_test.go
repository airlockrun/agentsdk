package connector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testSettings struct {
	Endpoint string        `connector:"url,required"`
	Token    Secret        `connector:"secret,required"`
	Workers  int64         `connector:"integer,default=2"`
	Timeout  time.Duration `connector:"duration,default=5s"`
}

func TestConfigureSettingsTransactional(t *testing.T) {
	settings := &testSettings{Endpoint: "https://old.example", Token: "old"}
	_, fields, err := settingsSchema(settings)
	if err != nil {
		t.Fatal(err)
	}
	err = configureSettings(context.Background(), settings, fields, []string{"--non-interactive", "--endpoint", "https://new.example", "--token-stdin"}, bytes.NewBufferString("new\n"), &bytes.Buffer{}, false, func(context.Context) error {
		return errors.New("unhealthy")
	})
	if err == nil {
		t.Fatal("configureSettings succeeded")
	}
	if settings.Endpoint != "https://old.example" || settings.Token != "old" {
		t.Fatalf("settings changed on failed self-test: %+v", settings)
	}
}

func TestConfigureSettingsAllowsExplicitEmptyAndTrimsCRLF(t *testing.T) {
	settings := &struct {
		Name  string `connector:"string"`
		Token Secret `connector:"secret"`
	}{Name: "old", Token: "old"}
	_, fields, err := settingsSchema(settings)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("new\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configureSettings(context.Background(), settings, fields, []string{"--non-interactive", "--name=", "--token-file", secret}, bytes.NewBuffer(nil), &bytes.Buffer{}, false, nil); err != nil {
		t.Fatal(err)
	}
	if settings.Name != "" || settings.Token != "new" {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestSettingsSchemaRejectsDuplicateJSONNames(t *testing.T) {
	typeOf := reflect.StructOf([]reflect.StructField{
		{Name: "First", Type: reflect.TypeOf(""), Tag: `json:"same"`},
		{Name: "Second", Type: reflect.TypeOf(""), Tag: `json:"same"`},
	})
	_, _, err := settingsSchema(reflect.New(typeOf).Interface())
	if err == nil || !strings.Contains(err.Error(), "duplicate setting JSON name") {
		t.Fatalf("error = %v", err)
	}
}

func TestSettingsSchemaRejectsArchitectureSizedIntegers(t *testing.T) {
	type namedInt int
	tests := []struct {
		name     string
		settings any
	}{
		{name: "inferred int", settings: &struct{ Workers int }{}},
		{name: "tagged int", settings: &struct {
			Workers int `connector:"integer"`
		}{}},
		{name: "uint", settings: &struct {
			Workers uint `connector:"integer"`
		}{}},
		{name: "uintptr", settings: &struct {
			Workers uintptr `connector:"integer"`
		}{}},
		{name: "named int", settings: &struct{ Workers namedInt }{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := settingsSchema(test.settings)
			if err == nil || !strings.Contains(err.Error(), "setting Workers") || !strings.Contains(err.Error(), "architecture-sized integer") || !strings.Contains(err.Error(), "fixed-width signed integer") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSettingsSchemaAcceptsFixedWidthIntegers(t *testing.T) {
	_, _, err := settingsSchema(&struct {
		Small   int32
		Large   int64
		Timeout time.Duration
	}{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryRootBindingsTrackConfiguredDirectorySettings(t *testing.T) {
	oldRoot, newRoot := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	settings := &struct {
		Root string `connector:"directory"`
	}{Root: oldRoot}
	runtime := New(Config{Kind: "roots", Contract: DefineContract("io.airlockrun.root_settings"), Name: "Roots", Description: "Root settings.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: settings, ServiceMode: ServiceUser, StateDirectory: t.TempDir()})
	directory := DefineDirectory(runtime.config.Contract, "files", DirectoryOptions{Revision: 1, Write: true})
	provider := LocalDirectory(settings.Root)
	directory.Handle(runtime, provider)
	bindings := runtime.directoryRootBindings()
	settings.Root = newRoot
	if err := bindings.apply(settings); err != nil {
		t.Fatal(err)
	}
	if provider.path != newRoot {
		t.Fatalf("provider path = %q, want %q", provider.path, newRoot)
	}
	if err := bindings.restore(); err != nil {
		t.Fatal(err)
	}
	if provider.path != oldRoot {
		t.Fatalf("restored provider path = %q, want %q", provider.path, oldRoot)
	}
}

type upgradeOldSettings struct {
	Endpoint string `connector:"url,required"`
	Removed  string `connector:"string"`
	Changed  int64  `connector:"integer"`
}

type upgradeCandidateSettings struct {
	Endpoint string `connector:"url,required"`
	Added    string `connector:"string,required"`
	Changed  string `connector:"string,required"`
}

func newSettingsUpgradeRuntime(t *testing.T, input string, selfTest func(context.Context) error) (*Runtime, *upgradeCandidateSettings, []byte, []byte) {
	t.Helper()
	base := t.TempDir()
	installationID := "00000000-0000-0000-0000-000000000001"
	stateDir := filepath.Join(base, "installations", installationID)
	if err := saveInstallation(filepath.Join(stateDir, "installation.json"), installationState{ServiceMode: ServiceUser, InstallationID: installationID, Credential: strings.Repeat("c", 32), Enabled: true}, false); err != nil {
		t.Fatal(err)
	}
	old := &upgradeOldSettings{Endpoint: "https://old.example", Removed: "drop-me", Changed: 7}
	descriptors, fields, err := settingsSchema(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(filepath.Join(stateDir, "settings.json"), old, false); err != nil {
		t.Fatal(err)
	}
	if err := saveSettingsSchema(filepath.Join(stateDir, "settings-schema.json"), descriptors, fields); err != nil {
		t.Fatal(err)
	}
	oldSettings, err := os.ReadFile(filepath.Join(stateDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldSchema, err := os.ReadFile(filepath.Join(stateDir, "settings-schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := &upgradeCandidateSettings{}
	previousArgs := os.Args
	os.Args = []string{previousArgs[0], "upgrade"}
	defer func() { os.Args = previousArgs }()
	runtime := New(Config{
		Kind: "upgrade-test", Contract: DefineContract("io.airlockrun.settings_upgrade"), Name: "Upgrade", Description: "Settings upgrade test.", ArtifactVersion: "2",
		Targets: []string{PlatformLinuxAMD64}, Settings: candidate, ServiceMode: ServiceUser, StateDirectory: base,
		Input: bytes.NewBufferString(input), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}, SelfTest: selfTest,
	})
	return runtime, candidate, oldSettings, oldSchema
}

func TestUpgradeSettingsAddsRequiredAndMigratesChangedSchema(t *testing.T) {
	runtime, candidate, _, _ := newSettingsUpgradeRuntime(t, "", nil)
	activate, cleanup, err := runtime.prepareUpgradeSettings(context.Background(), []string{"--non-interactive", "--added", "new", "--changed", "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if candidate.Endpoint != "https://old.example" || candidate.Added != "new" || candidate.Changed != "replacement" {
		t.Fatalf("candidate settings = %+v", candidate)
	}
	if err := activate(); err != nil {
		t.Fatal(err)
	}
	var saved upgradeCandidateSettings
	if err := loadSettings(filepath.Join(runtime.stateDir, "settings.json"), &saved, false); err != nil {
		t.Fatal(err)
	}
	if saved != *candidate {
		t.Fatalf("saved settings = %+v, want %+v", saved, *candidate)
	}
}

func TestUpgradeSettingsNonTTYReportsRequiredFlag(t *testing.T) {
	runtime, _, oldSettings, _ := newSettingsUpgradeRuntime(t, "", nil)
	_, _, err := runtime.prepareUpgradeSettings(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "required setting added is missing; provide --added") {
		t.Fatalf("error = %v", err)
	}
	current, readErr := os.ReadFile(filepath.Join(runtime.stateDir, "settings.json"))
	if readErr != nil || !bytes.Equal(current, oldSettings) {
		t.Fatalf("active settings changed: %s, %v", current, readErr)
	}
}

func TestNonTTYRequiredSecretReportsSafeFlags(t *testing.T) {
	settings := &struct {
		Token Secret `connector:"secret,required"`
	}{}
	_, fields, err := settingsSchema(settings)
	if err != nil {
		t.Fatal(err)
	}
	err = configureSettingsCommand(context.Background(), "upgrade", settings, fields, nil, bytes.NewBuffer(nil), &bytes.Buffer{}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "provide --token-file or --token-stdin") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeSettingsSelfTestFailurePreservesInstalledState(t *testing.T) {
	runtime, _, oldSettings, oldSchema := newSettingsUpgradeRuntime(t, "", func(context.Context) error { return errors.New("candidate unavailable") })
	_, _, err := runtime.prepareUpgradeSettings(context.Background(), []string{"--non-interactive", "--added", "new", "--changed", "replacement"})
	if err == nil || !strings.Contains(err.Error(), "candidate unavailable") {
		t.Fatalf("error = %v", err)
	}
	settings, settingsErr := os.ReadFile(filepath.Join(runtime.stateDir, "settings.json"))
	schema, schemaErr := os.ReadFile(filepath.Join(runtime.stateDir, "settings-schema.json"))
	if settingsErr != nil || schemaErr != nil || !bytes.Equal(settings, oldSettings) || !bytes.Equal(schema, oldSchema) {
		t.Fatalf("installed state changed: settings=%s schema=%s errors=%v/%v", settings, schema, settingsErr, schemaErr)
	}
}
