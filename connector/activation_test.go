package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

func TestNonTTYActivationSavesPendingAndExits(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/connectors/v1/enroll/device-code" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(protocol.DeviceCodeResponse{
			DeviceSecret: strings.Repeat("d", 32), UserCode: "ABCD-EFGH", VerificationURL: serverURL(r),
			ExpiresAt: time.Now().Add(time.Minute), PollIntervalSeconds: 1,
		})
	}))
	t.Cleanup(server.Close)
	stateDir := t.TempDir()
	output := &bytes.Buffer{}
	runtime := testRuntime(stateDir, output, server.Client())
	if err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", server.URL}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	state, err := loadInstallation(filepath.Join(draftStateDirectory(stateDir), "installation.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.DeviceSecret != strings.Repeat("d", 32) {
		t.Fatalf("pending state = %+v", state.Pending)
	}
	if !bytes.Contains(output.Bytes(), []byte("--check")) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestActivationRequiresConfiguration(t *testing.T) {
	base := t.TempDir()
	runtime := New(Config{
		Kind: "unconfigured", Contract: DefineContract("io.airlockrun.unconfigured"), Name: "Unconfigured", Description: "Unconfigured connector.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser, StateDirectory: base, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}, AllowInsecureHTTP: true,
	})
	err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", "http://127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "configure the connector before activation") {
		t.Fatalf("error = %v", err)
	}
}

func TestActivationRequiresConfiguredSettingsSnapshot(t *testing.T) {
	base := t.TempDir()
	if err := saveSettingsSchema(filepath.Join(draftStateDirectory(base), "settings-schema.json"), nil, nil); err != nil {
		t.Fatal(err)
	}
	runtime := New(Config{
		Kind: "missing-settings", Contract: DefineContract("io.airlockrun.missing_settings"), Name: "Missing", Description: "Missing settings.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser, StateDirectory: base, Input: bytes.NewBuffer(nil), Output: &bytes.Buffer{}, ErrorOutput: &bytes.Buffer{}, AllowInsecureHTTP: true,
	})
	err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", "http://127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "settings snapshot is missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestActivationCheckStates(t *testing.T) {
	tests := []struct {
		name, status string
		expired      bool
		want         string
	}{
		{name: "pending", status: "pending", want: "still pending"},
		{name: "denied", status: "denied", want: "denied"},
		{name: "expired response", status: "expired", want: "expired"},
		{name: "expired locally", expired: true, want: "expired"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(protocol.EnrollmentResponse{Status: test.status})
			}))
			defer server.Close()
			stateDir := t.TempDir()
			runtime := testRuntime(stateDir, &bytes.Buffer{}, server.Client())
			expires := time.Now().Add(time.Minute)
			if test.expired {
				expires = time.Now().Add(-time.Second)
			}
			state := installationState{Version: 1, ServiceMode: ServiceUser, Enabled: true, AirlockURL: server.URL, Pending: &pendingActivation{DeviceSecret: "secret", UserCode: "CODE", VerificationURL: server.URL, ExpiresAt: expires, PollInterval: 1}}
			if err := saveInstallation(filepath.Join(draftStateDirectory(stateDir), "installation.json"), state, false); err != nil {
				t.Fatal(err)
			}
			err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", server.URL, "--check"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestActivationRejectsOriginDrift(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.EnrollmentResponse{Status: "approved", InstallationID: "00000000-0000-0000-0000-000000000001", Credential: strings.Repeat("c", 32), StorageOrigins: []string{"https://other.example"}})
	}))
	defer server.Close()
	stateDir := t.TempDir()
	runtime := testRuntime(stateDir, &bytes.Buffer{}, server.Client())
	state := installationState{Version: 1, ServiceMode: ServiceUser, Enabled: true, AirlockURL: server.URL, Pending: &pendingActivation{DeviceSecret: "secret", UserCode: "CODE", VerificationURL: server.URL, ExpiresAt: time.Now().Add(time.Minute), PollInterval: 1, StorageOrigins: []string{"https://storage.example"}}}
	if err := saveInstallation(filepath.Join(draftStateDirectory(stateDir), "installation.json"), state, false); err != nil {
		t.Fatal(err)
	}
	err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", server.URL, "--check"})
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("error = %v", err)
	}
}

func TestActivationRejectsRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	runtime := testRuntime(t.TempDir(), &bytes.Buffer{}, source.Client())
	err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", source.URL})
	if err == nil {
		t.Fatal("activation followed redirect")
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect destination was called")
	}
}

func TestActivationCompletesDurableLocalApprovalWithoutPolling(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	base := t.TempDir()
	runtime := testRuntime(base, &bytes.Buffer{}, server.Client())
	id := "00000000-0000-0000-0000-000000000001"
	state := installationState{Version: 1, ServiceMode: ServiceUser, Enabled: true, AirlockURL: server.URL, Pending: &pendingActivation{
		DeviceSecret: "secret", ExpiresAt: time.Now().Add(time.Minute), PollInterval: 1,
		Approved: &approvedActivation{InstallationID: id, Credential: strings.Repeat("c", 32)},
	}}
	if err := saveInstallation(filepath.Join(draftStateDirectory(base), "installation.json"), state, false); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", server.URL, "--check"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("approval recovery made %d server requests", requests.Load())
	}
	saved, err := loadInstallation(filepath.Join(base, "installations", id, "installation.json"), false)
	if err != nil || saved.Credential == "" || saved.Pending != nil {
		t.Fatalf("saved state = %+v, error = %v", saved, err)
	}
}

func TestActivationPollingDoesNotHoldDraftLock(t *testing.T) {
	base := t.TempDir()
	var acquired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		lock, err := acquireFileLock(ctx, filepath.Join(draftStateDirectory(base), ".installation.lock"))
		if err != nil {
			t.Errorf("poll held draft lock: %v", err)
			return
		}
		acquired.Store(true)
		_ = lock.Close()
		_ = json.NewEncoder(w).Encode(protocol.EnrollmentResponse{Status: "pending"})
	}))
	defer server.Close()
	runtime := testRuntime(base, &bytes.Buffer{}, server.Client())
	state := installationState{Version: 1, ServiceMode: ServiceUser, Enabled: true, AirlockURL: server.URL, Pending: &pendingActivation{DeviceSecret: "secret", ExpiresAt: time.Now().Add(time.Minute), PollInterval: 1}}
	if err := saveInstallation(filepath.Join(draftStateDirectory(base), "installation.json"), state, false); err != nil {
		t.Fatal(err)
	}
	err := runtime.RunContext(context.Background(), []string{"activate", "--airlock", server.URL, "--check"})
	if err == nil || !strings.Contains(err.Error(), "still pending") || !acquired.Load() {
		t.Fatalf("activation error = %v, acquired = %t", err, acquired.Load())
	}
}

func testRuntime(stateDir string, output *bytes.Buffer, client *http.Client) *Runtime {
	if err := saveSettingsSchema(filepath.Join(draftStateDirectory(stateDir), "settings-schema.json"), nil, nil); err != nil {
		panic(err)
	}
	if err := saveSettings(filepath.Join(draftStateDirectory(stateDir), "settings.json"), &struct{}{}, false); err != nil {
		panic(err)
	}
	runtime := New(Config{
		Kind: "test", Contract: DefineContract("io.airlockrun.connector_test"), Name: "Test", Description: "Test connector.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser, StateDirectory: stateDir, HTTPClient: client, AllowInsecureHTTP: true,
		Input: bytes.NewBuffer(nil), Output: output, ErrorOutput: output,
	})
	if err := runtime.initialize([]string{"version"}); err != nil {
		panic(err)
	}
	return runtime
}

func serverURL(r *http.Request) string { return "http://" + r.Host + "/connect" }
