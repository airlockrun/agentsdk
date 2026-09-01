package connector

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testSettings struct {
	Endpoint string        `json:"endpoint" connector:"url,required"`
	Token    Secret        `json:"token" connector:"secret,required"`
	Workers  int64         `json:"workers" connector:"integer,default=2"`
	Timeout  time.Duration `json:"timeout" connector:"duration,default=5s"`
}

func TestHostSettingsAreStrictDefaultedAndPublished(t *testing.T) {
	settings := DefineSettings[testSettings]()
	if got := settings.descriptors[0]; got.Name != "endpoint" || got.JSONName != "endpoint" {
		t.Fatalf("endpoint descriptor = %+v", got)
	}
	var seen testSettings
	runtime := New(Config{Kind: "settings", Contract: DefineContract("io.airlockrun.settings"), Name: "Settings", Description: "Settings test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: settings, Validate: func(context.Context) error { seen = settings.Get(); return nil }})
	if err := runtime.applySettings(context.Background(), []byte(`{"endpoint":"https://example.com","token":"secret"}`), nil); err != nil {
		t.Fatal(err)
	}
	if seen.Workers != 2 || seen.Timeout != 5*time.Second {
		t.Fatalf("settings = %+v", seen)
	}
	if err := runtime.applySettings(context.Background(), []byte(`{"endpoint":"https://example.com","token":"secret","unknown":true}`), nil); err == nil {
		t.Fatal("unknown setting accepted")
	}
}

func TestSettingsPublishExactJSONNameSeparatelyFromDisplayName(t *testing.T) {
	settings := DefineSettings[struct {
		BrokerURL string `json:"broker.url" connector:"url,name=mqtt-broker,required"`
	}]()
	if got := settings.descriptors[0]; got.Name != "mqtt-broker" || got.JSONName != "broker.url" {
		t.Fatalf("descriptor = %+v", got)
	}
	var seen string
	runtime := New(Config{Kind: "settings-json-name", Contract: DefineContract("io.airlockrun.settings_json_name"), Name: "Settings", Description: "Settings test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: settings, Validate: func(context.Context) error { seen = settings.Get().BrokerURL; return nil }})
	manifest, err := json.Marshal(runtime.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"jsonName":"broker.url"`) {
		t.Fatalf("manifest = %s", manifest)
	}
	if err := runtime.applySettings(context.Background(), []byte(`{"broker.url":"https://example.com"}`), nil); err != nil {
		t.Fatal(err)
	}
	if seen != "https://example.com" {
		t.Fatalf("broker URL = %q", seen)
	}
}

func TestHostSettingsValidateDeclaredKinds(t *testing.T) {
	optional := DefineSettings[struct {
		Mode     string `json:"mode" connector:"enum,enum=read|write"`
		Endpoint string `json:"endpoint" connector:"url"`
	}]()
	optionalRuntime := New(Config{Kind: "optional-settings", Contract: DefineContract("io.airlockrun.optional_settings"), Name: "Settings", Description: "Optional settings test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: optional})
	if err := optionalRuntime.applySettings(context.Background(), []byte(`{}`), nil); err != nil {
		t.Fatalf("omitted optional settings: %v", err)
	}

	tests := []struct {
		name string
		raw  string
	}{
		{name: "enum", raw: `{"mode":"invalid","endpoint":"https://example.com","directory":"/tmp"}`},
		{name: "URL", raw: `{"mode":"read","endpoint":"not-a-url","directory":"/tmp"}`},
		{name: "directory", raw: `{"mode":"read","endpoint":"https://example.com","directory":"relative"}`},
		{name: "null", raw: `{"mode":"read","endpoint":null,"directory":"/tmp"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := DefineSettings[struct {
				Mode      string `json:"mode" connector:"enum,enum=read|write"`
				Endpoint  string `json:"endpoint" connector:"url"`
				Directory string `json:"directory" connector:"directory"`
			}]()
			runtime := New(Config{Kind: "semantic-settings", Contract: DefineContract("io.airlockrun.semantic_settings"), Name: "Settings", Description: "Semantic settings test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: settings})
			if err := runtime.applySettings(context.Background(), []byte(test.raw), nil); err == nil {
				t.Fatal("invalid host setting accepted")
			}
		})
	}
}

func TestExplicitZeroHostSettingsOverrideDefaults(t *testing.T) {
	settings := DefineSettings[struct {
		Enabled bool   `json:"enabled" connector:"bool,default=true"`
		Workers int64  `json:"workers" connector:"integer,default=4"`
		Label   string `json:"label" connector:"string,default=fallback"`
	}]()
	var seen struct {
		Enabled bool
		Workers int64
		Label   string
	}
	runtime := New(Config{
		Kind: "zero-settings", Contract: DefineContract("io.airlockrun.zero_settings"), Name: "Settings", Description: "Zero settings test.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, Settings: settings,
		Validate: func(context.Context) error {
			got := settings.Get()
			seen.Enabled, seen.Workers, seen.Label = got.Enabled, got.Workers, got.Label
			return nil
		},
	})
	if err := runtime.applySettings(context.Background(), []byte(`{"enabled":false,"workers":0,"label":""}`), nil); err != nil {
		t.Fatal(err)
	}
	if seen.Enabled || seen.Workers != 0 || seen.Label != "" {
		t.Fatalf("settings = %+v", seen)
	}
}

func TestFailedHostSettingsRestorePublishedSnapshot(t *testing.T) {
	settings := DefineSettings[testSettings]()
	reject := false
	runtime := New(Config{
		Kind: "settings-restore", Contract: DefineContract("io.airlockrun.settings_restore"), Name: "Settings", Description: "Settings restore test.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, Settings: settings,
		Validate: func(context.Context) error {
			if reject {
				return errors.New("rejected")
			}
			return nil
		},
	})
	if err := runtime.applySettings(context.Background(), []byte(`{"endpoint":"https://one.example","token":"one"}`), nil); err != nil {
		t.Fatal(err)
	}
	reject = true
	if err := runtime.applySettings(context.Background(), []byte(`{"endpoint":"https://two.example","token":"two"}`), nil); err == nil {
		t.Fatal("invalid settings accepted")
	}
	if got := settings.Get(); got.Endpoint != "https://one.example" || got.Token != "one" {
		t.Fatalf("restored settings = %+v", got)
	}
}

func TestSettingsHandleBelongsToOneRuntime(t *testing.T) {
	settings := DefineSettings[testSettings]()
	config := Config{Kind: "owner", Contract: DefineContract("io.airlockrun.settings_owner"), Name: "Owner", Description: "Settings owner.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Settings: settings}
	New(config)
	assertPanicContains(t, "only one runtime", func() { New(config) })
}

func TestSettingsSchemaRejectsArchitectureSizedIntegers(t *testing.T) {
	for _, value := range []any{&struct{ Workers int }{}, &struct{ Workers uint }{}, &struct{ Workers uintptr }{}} {
		if _, _, err := settingsSchema(value); err == nil || !strings.Contains(err.Error(), "architecture-sized integer") {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestSettingsSchemaRejectsDuplicateJSONNames(t *testing.T) {
	typeOf := reflect.StructOf([]reflect.StructField{{Name: "First", Type: reflect.TypeOf(""), Tag: `json:"same"`}, {Name: "Second", Type: reflect.TypeOf(""), Tag: `json:"same"`}})
	if _, _, err := settingsSchema(reflect.New(typeOf).Interface()); err == nil || !strings.Contains(err.Error(), "duplicate setting JSON name") {
		t.Fatalf("error = %v", err)
	}
}
