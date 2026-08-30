package connector

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestOnStartRunsInRegistrationOrder(t *testing.T) {
	runtime := New(Config{Kind: "hooks", Contract: DefineContract("io.airlockrun.hooks"), Name: "Hooks", Description: "Startup hooks.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser})
	var calls []string
	runtime.OnStart("first", func(context.Context) error { calls = append(calls, "first"); return nil })
	runtime.OnStart("second", func(context.Context) error { calls = append(calls, "second"); return nil })
	if err := runtime.runStartHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestOnStartFailsLoudly(t *testing.T) {
	runtime := New(Config{Kind: "hooks", Contract: DefineContract("io.airlockrun.hook_errors"), Name: "Hooks", Description: "Startup hooks.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser})
	want := errors.New("unavailable")
	runtime.OnStart("dependency", func(context.Context) error { return want })
	if err := runtime.runStartHooks(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	assertPanicContains(t, "duplicate OnStart", func() {
		runtime.OnStart("dependency", func(context.Context) error { return nil })
	})
}

func TestRunBindsSettingsBeforeStartAndJoinsRuntimeGoroutines(t *testing.T) {
	type runtimeSettings struct {
		Value string `connector:"string,required"`
	}
	interfacePublished := make(chan struct{})
	heartbeatSent := make(chan struct{})
	var interfaceOnce, heartbeatOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/connectors/v1/interface":
			interfaceOnce.Do(func() { close(interfacePublished) })
		case "/api/connectors/v1/heartbeat":
			heartbeatOnce.Do(func() { close(heartbeatSent) })
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	base := t.TempDir()
	id := "00000000-0000-0000-0000-000000000001"
	stateDirectory := filepath.Join(base, "installations", id)
	if err := saveInstallation(filepath.Join(stateDirectory, "installation.json"), installationState{
		Version: 1, ServiceMode: ServiceUser, InstallationID: id, Credential: strings.Repeat("c", 32), AirlockURL: server.URL, Enabled: true,
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(filepath.Join(stateDirectory, "settings.json"), &runtimeSettings{Value: "configured"}, false); err != nil {
		t.Fatal(err)
	}

	settings := DefineSettings[runtimeSettings]()
	runtime := New(Config{
		Kind: "run-lifecycle", Contract: DefineContract("io.airlockrun.run_lifecycle"), Name: "Run", Description: "Run lifecycle.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, Settings: settings, ServiceMode: ServiceUser, StateDirectory: base,
		HTTPClient: server.Client(), AllowInsecureHTTP: true, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{},
	})
	started := make(chan struct{})
	stopped := make(chan struct{})
	runtime.OnStart("dependency", func(ctx context.Context) error {
		if settings.Get().Value != "configured" {
			return errors.New("startup received wrong settings")
		}
		close(started)
		go func() {
			<-ctx.Done()
			close(stopped)
		}()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		<-interfacePublished
		<-heartbeatSent
		cancel()
	}()
	err := runtime.RunContext(ctx, []string{"run", "--installation", id})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-stopped
	assertPanicContains(t, "unavailable during connector definition", func() { settings.Get() })
}
