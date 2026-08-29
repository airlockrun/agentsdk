package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

type runtimeInput struct {
	Value string `json:"value"`
}

func TestManifestUsesRunningExecutableDigest(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := fileDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	manifest := runtime.Manifest()
	if manifest.ArtifactDigest != want {
		t.Fatalf("artifact digest = %q, want %q", manifest.ArtifactDigest, want)
	}
	if err := protocol.ValidateArtifactDigest(manifest.ArtifactDigest); err != nil {
		t.Fatal(err)
	}
}

func TestServiceLifecycleGuard(t *testing.T) {
	tests := []struct {
		name, command      string
		enabled, installed bool
		want               string
	}{
		{name: "fresh install", command: "install", enabled: true},
		{name: "installed install", command: "install", enabled: true, installed: true, want: "use upgrade"},
		{name: "enabled upgrade", command: "upgrade", enabled: true, installed: true},
		{name: "disabled upgrade", command: "upgrade", installed: true, want: "enable it before upgrade"},
		{name: "disabled rollback", command: "rollback", installed: true, want: "enable it before rollback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := serviceLifecycleGuard(test.command, installationState{Enabled: test.enabled}, test.installed)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCommandDispatchConcurrentDedupe(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "dedupe", CommandOptions{Revision: 1})
	started, release := make(chan struct{}), make(chan struct{})
	calls := 0
	var callsMu sync.Mutex
	command.Handle(runtime, func(_ Context, input runtimeInput) (runtimeOutput, error) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		close(started)
		<-release
		return runtimeOutput{Value: input.Value}, nil
	})
	raw, _ := json.Marshal(runtimeInput{Value: "once"})
	base := protocol.JobRequest{JobID: "job-1", AttemptToken: "attempt-1", IdempotencyID: "same", Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: 1, Mode: command.Mode(), InputSchemaHash: command.Descriptor().InputSchemaHash, OutputSchemaHash: command.Descriptor().OutputSchemaHash, Input: raw, Deadline: time.Now().Add(time.Minute)}
	results := make(chan error, 2)
	go func() {
		_, err := runtime.dispatchCommand(&executionContext{Context: context.Background(), job: base}, base)
		results <- err
	}()
	<-started
	second := base
	second.JobID, second.AttemptToken = "job-2", "attempt-2"
	go func() {
		_, err := runtime.dispatchCommand(&executionContext{Context: context.Background(), job: second}, second)
		results <- err
	}()
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 1 {
		t.Fatalf("handler calls = %d", calls)
	}
}

func TestIndeterminateIdempotencyRecordFailsLoud(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	job := protocol.JobRequest{JobID: "new-job", AttemptToken: "new-attempt", IdempotencyID: "crashed"}
	path := runtime.idempotencyPath(job.IdempotencyID)
	if err := runtime.saveIdempotency(path, idempotencyRecord{Version: 1, Status: "indeterminate", JobID: "old-job", AttemptToken: "old-attempt", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.executeOnce(context.Background(), job, func() (json.RawMessage, error) { t.Fatal("execute called"); return nil, nil }); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("error = %v", err)
	}
}

func TestIdempotencyAdmissionPreservesProtectedTerminalRecords(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	terminalPath := runtime.idempotencyPath("terminal")
	if err := runtime.saveIdempotency(terminalPath, idempotencyRecord{Version: 1, Status: "completed", JobID: "job", AttemptToken: "attempt", Output: json.RawMessage(`{}`), CreatedAt: time.Now(), ReservedBytes: maxIdempotencyBytes}); err != nil {
		t.Fatal(err)
	}
	job := protocol.JobRequest{JobID: "new", AttemptToken: "new-attempt", IdempotencyID: "new"}
	called := false
	_, err := runtime.executeOnce(context.Background(), job, func() (json.RawMessage, error) { called = true; return json.RawMessage(`{}`), nil })
	if err == nil || !strings.Contains(err.Error(), "storage is full") || called {
		t.Fatalf("executeOnce error = %v, called = %t", err, called)
	}
	if _, err := os.Stat(terminalPath); err != nil {
		t.Fatalf("protected terminal record was evicted: %v", err)
	}
	if _, _, err := runtime.compactIdempotency(context.Background(), time.Now().Add(idempotencyRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(terminalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired terminal record still exists: %v", err)
	}
}

func TestCommandTimeoutAfterHandlerReturnsIsDurablyFailed(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "late", CommandOptions{Revision: 1, Timeout: time.Millisecond})
	command.Handle(runtime, func(Context, runtimeInput) (runtimeOutput, error) {
		time.Sleep(5 * time.Millisecond)
		return runtimeOutput{Value: "late"}, nil
	})
	raw, _ := json.Marshal(runtimeInput{})
	descriptor := command.Descriptor()
	job := protocol.JobRequest{JobID: "job", AttemptToken: "attempt", IdempotencyID: "late", Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: 1, Mode: command.Mode(), InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: raw, Deadline: time.Now().Add(time.Minute)}
	_, err := runtime.dispatchCommand(&executionContext{Context: context.Background(), job: job}, job)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	record, found, loadErr := runtime.loadIdempotency(runtime.idempotencyPath("late"))
	if loadErr != nil || !found || record.Status != "failed" {
		t.Fatalf("record = %+v, found = %t, error = %v", record, found, loadErr)
	}
}

func TestProgressRequiresJobMode(t *testing.T) {
	ctx := &executionContext{Context: context.Background(), mode: protocol.CommandModeUnary}
	if err := ctx.Progress("phase", "", 0, 0); err == nil {
		t.Fatal("unary progress succeeded")
	}
}

func TestProtocolClientRejectsRedirectWithoutForwardingCredential(t *testing.T) {
	var authorization string
	var authorizationMu sync.Mutex
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationMu.Lock()
		authorization = r.Header.Get("Authorization")
		authorizationMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := newProtocolClient(source.URL, "secret", source.Client(), true)
	if err != nil {
		t.Fatal(err)
	}
	err = client.post(context.Background(), "/redirect", struct{}{}, nil)
	if err == nil {
		t.Fatal("protocol client followed redirect")
	}
	authorizationMu.Lock()
	defer authorizationMu.Unlock()
	if authorization != "" {
		t.Fatalf("credential forwarded: %q", authorization)
	}
}

func TestLongJobProgressUsesAttemptEnvelope(t *testing.T) {
	var event protocol.JobEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := newProtocolClient(server.URL, "credential", server.Client(), true)
	if err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "long_job", CommandOptions{Revision: 1, Mode: protocol.CommandModeJob})
	command.Handle(runtime, func(ctx Context, input runtimeInput) (runtimeOutput, error) {
		if err := ctx.Progress("work", "running", 1, 2); err != nil {
			return runtimeOutput{}, err
		}
		return runtimeOutput{Value: input.Value}, nil
	})
	raw, _ := json.Marshal(runtimeInput{Value: "done"})
	job := protocol.JobRequest{JobID: "job", AttemptToken: "attempt", IdempotencyID: "idempotency", Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: 1, Mode: command.Mode(), InputSchemaHash: command.Descriptor().InputSchemaHash, OutputSchemaHash: command.Descriptor().OutputSchemaHash, Input: raw, Deadline: time.Now().Add(time.Minute)}
	if _, err := runtime.dispatchCommand(&executionContext{Context: context.Background(), job: job, client: client}, job); err != nil {
		t.Fatal(err)
	}
	if event.AttemptToken != "attempt" || event.Sequence != 1 || event.Phase != "work" {
		t.Fatalf("event = %+v", event)
	}
}

func TestHeartbeatTracksHealthTransitions(t *testing.T) {
	requests := make(chan protocol.HeartbeatRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request protocol.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests <- request
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	var unhealthy atomic.Bool
	unhealthy.Store(true)
	runtime := New(Config{Kind: "health", Contract: DefineContract("io.airlockrun.health_test"), Name: "Health", Description: "Health connector.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, ServiceMode: ServiceUser, StateDirectory: t.TempDir(), HTTPClient: server.Client(), AllowInsecureHTTP: true, SelfTest: func(context.Context) error {
		if unhealthy.Load() {
			return errors.New("dependency down")
		}
		return nil
	}})
	client, _ := newProtocolClient(server.URL, "credential", server.Client(), true)
	runtime.config.HeartbeatInterval = 10 * time.Millisecond
	runtime.active[activeAttemptKey{jobID: "job-b", attemptToken: "attempt-b"}] = activeJob{}
	runtime.active[activeAttemptKey{jobID: "job-a", attemptToken: "attempt-a"}] = activeJob{}
	ctx, cancel := context.WithCancel(context.Background())
	go runtime.heartbeat(ctx, client, filepath.Join(runtime.stateDir, "runtime.json"))
	first := <-requests
	if first.Readiness != protocol.ReadinessUnhealthy || runtime.healthy.Load() {
		t.Fatalf("first heartbeat = %+v", first)
	}
	if len(first.ActiveAttempts) != 2 || first.ActiveAttempts[0].JobID != "job-a" || first.ActiveAttempts[0].AttemptToken != "attempt-a" || first.ActiveAttempts[1].JobID != "job-b" {
		t.Fatalf("first active attempts = %+v", first.ActiveAttempts)
	}
	if first.ArtifactDigest != runtime.Manifest().ArtifactDigest {
		t.Fatalf("heartbeat artifact digest = %q, want %q", first.ArtifactDigest, runtime.Manifest().ArtifactDigest)
	}
	unhealthy.Store(false)
	second := <-requests
	cancel()
	if second.Readiness != protocol.ReadinessReady || !runtime.healthy.Load() {
		t.Fatalf("second heartbeat = %+v", second)
	}
}

type rollbackTestManager struct {
	calls        []string
	statusPath   string
	digest       string
	upgradeReady bool
	upgradeErr   error
}

func (m *rollbackTestManager) PrepareIdentity(context.Context) error {
	m.calls = append(m.calls, "prepare-identity")
	return nil
}
func (*rollbackTestManager) ValidateIdentity(context.Context, string, string) error { return nil }
func (*rollbackTestManager) Install(context.Context) error                          { return nil }
func (*rollbackTestManager) Uninstall(context.Context) error                        { return nil }
func (*rollbackTestManager) Start(context.Context) error                            { return nil }
func (m *rollbackTestManager) Stop(context.Context) error {
	m.calls = append(m.calls, "stop")
	return nil
}
func (*rollbackTestManager) Status(context.Context) (string, error) { return "active", nil }
func (*rollbackTestManager) Reconfigure(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}
func (m *rollbackTestManager) Upgrade(context.Context, func() error) (bool, error) {
	return m.upgradeReady, m.upgradeErr
}
func (*rollbackTestManager) Enable(context.Context) error  { return nil }
func (*rollbackTestManager) Disable(context.Context) error { return nil }
func (*rollbackTestManager) Installed() bool               { return true }
func (m *rollbackTestManager) Rollback(context.Context) error {
	m.calls = append(m.calls, "replace-start")
	return saveRuntimeStatus(m.statusPath, runtimeStatus{Readiness: protocol.ReadinessReady, ArtifactVersion: "old", ArtifactDigest: m.digest})
}
func (m *rollbackTestManager) RollbackDigest() (string, error) { return m.digest, nil }

func TestRollbackRestoresStateAndVerifiesPriorDigest(t *testing.T) {
	base := t.TempDir()
	installationID := "00000000-0000-0000-0000-000000000001"
	stateDir := filepath.Join(base, "installations", installationID)
	runtime := testRuntime(base, nilBuffer(), nil)
	runtime.stateDir, runtime.installationID = stateDir, installationID
	oldInstallation := []byte(`{"old":"installation"}`)
	oldSettings := []byte(`{"old":"settings"}`)
	oldSchema := []byte(`{"version":1,"settings":[]}`)
	if err := saveRollbackState(filepath.Join(stateDir, "rollback-state.json"), rollbackState{ServiceMode: ServiceUser, InstallationID: installationID, Installation: oldInstallation, Settings: oldSettings, SettingsPresent: true, SettingsSchema: oldSchema}); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(stateDir, "installation.json"), []byte(`{"new":"installation"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(stateDir, "settings.json"), []byte(`{"new":"settings"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(stateDir, "settings-schema.json"), []byte(`{"new":"schema"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	manager := &rollbackTestManager{statusPath: filepath.Join(stateDir, "runtime.json"), digest: digest}
	if err := runtime.rollbackService(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	if strings.Join(manager.calls, ",") != "stop,prepare-identity,replace-start" {
		t.Fatalf("rollback calls = %v", manager.calls)
	}
	installation, err := os.ReadFile(filepath.Join(stateDir, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(stateDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installation, oldInstallation) || !bytes.Equal(settings, oldSettings) {
		t.Fatalf("restored state = %s / %s", installation, settings)
	}
	schema, err := os.ReadFile(filepath.Join(stateDir, "settings-schema.json"))
	if err != nil || !bytes.Equal(schema, oldSchema) {
		t.Fatalf("restored schema = %s, %v", schema, err)
	}
}

func TestUpgradeRestoresPreviousGenericRollbackStateBeforeArtifactCommit(t *testing.T) {
	runtime, _, _, _ := newSettingsUpgradeRuntime(t, "", nil)
	path := filepath.Join(runtime.stateDir, "rollback-state.json")
	if err := saveRollbackState(path, rollbackState{
		ServiceMode: ServiceUser, InstallationID: runtime.installationID,
		Installation: []byte(`{"prior":"installation"}`), SettingsSchema: []byte(`{"prior":"schema"}`),
	}); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := &rollbackTestManager{upgradeErr: errors.New("artifact retention failed before pointer commit")}
	err = runtime.upgradeService(context.Background(), manager, []string{"--non-interactive", "--added", "new", "--changed", "replacement"})
	if err == nil || !strings.Contains(err.Error(), "artifact retention failed") {
		t.Fatalf("upgrade error = %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, previous) {
		t.Fatalf("generic rollback state was not restored: %s, want %s", restored, previous)
	}
}

func TestRollbackCommandFailsWithoutRetainedBinary(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	err := runtime.RunContext(context.Background(), []string{"rollback"})
	if err == nil || !strings.Contains(err.Error(), "retained rollback binary") {
		t.Fatalf("rollback error = %v", err)
	}
}

func TestReadinessFailsImmediatelyWhenCandidateExits(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	started := time.Now().Add(-time.Second)
	if err := saveRuntimeStatus(filepath.Join(runtime.stateDir, "runtime.json"), runtimeStatus{Readiness: protocol.ReadinessOffline, ArtifactVersion: runtime.config.ArtifactVersion, ArtifactDigest: runtime.artifactDigest}); err != nil {
		t.Fatal(err)
	}
	err := runtime.waitReady(context.Background(), started)
	if err == nil || !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("readiness error = %v", err)
	}
}

type failedReadinessUpgradeManager struct {
	runtime     *Runtime
	binaryPath  string
	oldBinary   []byte
	newBinary   []byte
	oldSettings []byte
	calls       []string
}

func (m *failedReadinessUpgradeManager) PrepareIdentity(context.Context) error { return nil }
func (*failedReadinessUpgradeManager) ValidateIdentity(context.Context, string, string) error {
	return nil
}
func (*failedReadinessUpgradeManager) Install(context.Context) error   { return nil }
func (*failedReadinessUpgradeManager) Uninstall(context.Context) error { return nil }
func (*failedReadinessUpgradeManager) Start(context.Context) error     { return nil }
func (m *failedReadinessUpgradeManager) Stop(context.Context) error {
	m.calls = append(m.calls, "stop")
	return nil
}
func (*failedReadinessUpgradeManager) Status(context.Context) (string, error) { return "stopped", nil }
func (*failedReadinessUpgradeManager) Reconfigure(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}
func (m *failedReadinessUpgradeManager) Upgrade(_ context.Context, activate func() error) (bool, error) {
	m.calls = append(m.calls, "stop-old")
	if err := atomicWrite(m.binaryPath+".rollback", m.oldBinary, 0o700); err != nil {
		return false, err
	}
	if err := atomicWrite(m.binaryPath, m.newBinary, 0o700); err != nil {
		return true, err
	}
	if err := activate(); err != nil {
		return true, err
	}
	m.calls = append(m.calls, "candidate-exited")
	err := saveRuntimeStatus(filepath.Join(m.runtime.stateDir, "runtime.json"), runtimeStatus{Readiness: protocol.ReadinessUnhealthy, ArtifactVersion: m.runtime.config.ArtifactVersion, ArtifactDigest: m.runtime.artifactDigest, Message: "candidate exited"})
	return true, err
}
func (*failedReadinessUpgradeManager) Enable(context.Context) error  { return nil }
func (*failedReadinessUpgradeManager) Disable(context.Context) error { return nil }
func (*failedReadinessUpgradeManager) Installed() bool               { return true }
func (m *failedReadinessUpgradeManager) Rollback(context.Context) error {
	settings, err := os.ReadFile(filepath.Join(m.runtime.stateDir, "settings.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(settings, m.oldSettings) {
		return errors.New("old binary would restart with candidate settings")
	}
	if err := atomicWrite(m.binaryPath, m.oldBinary, 0o700); err != nil {
		return err
	}
	m.calls = append(m.calls, "rollback-start")
	digest, err := fileDigest(m.binaryPath)
	if err != nil {
		return err
	}
	return saveRuntimeStatus(filepath.Join(m.runtime.stateDir, "runtime.json"), runtimeStatus{Readiness: protocol.ReadinessReady, ArtifactVersion: "1", ArtifactDigest: digest})
}
func (m *failedReadinessUpgradeManager) RollbackDigest() (string, error) {
	return fileDigest(m.binaryPath + ".rollback")
}

func TestFailedUpgradeReadinessRollsBackBinaryAndSettings(t *testing.T) {
	runtime, _, oldSettings, oldSchema := newSettingsUpgradeRuntime(t, "", nil)
	binaryPath := filepath.Join(runtime.stateDir, "test-binary")
	oldBinary, newBinary := []byte("old binary"), []byte("candidate binary")
	if err := atomicWrite(binaryPath, oldBinary, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &failedReadinessUpgradeManager{runtime: runtime, binaryPath: binaryPath, oldBinary: oldBinary, newBinary: newBinary, oldSettings: oldSettings}
	err := runtime.upgradeService(context.Background(), manager, []string{"--non-interactive", "--added", "new", "--changed", "replacement"})
	if err == nil || !strings.Contains(err.Error(), "candidate exited") {
		t.Fatalf("upgrade error = %v", err)
	}
	binary, binaryErr := os.ReadFile(binaryPath)
	settings, settingsErr := os.ReadFile(filepath.Join(runtime.stateDir, "settings.json"))
	schema, schemaErr := os.ReadFile(filepath.Join(runtime.stateDir, "settings-schema.json"))
	if binaryErr != nil || settingsErr != nil || schemaErr != nil || !bytes.Equal(binary, oldBinary) || !bytes.Equal(settings, oldSettings) || !bytes.Equal(schema, oldSchema) {
		t.Fatalf("rollback state: binary=%s settings=%s schema=%s errors=%v/%v/%v", binary, settings, schema, binaryErr, settingsErr, schemaErr)
	}
	if strings.Join(manager.calls, ",") != "stop-old,candidate-exited,stop,rollback-start" {
		t.Fatalf("upgrade calls = %v", manager.calls)
	}
}

type runtimeOutput struct {
	Value string `json:"value"`
}

func TestCommandDispatchDurablyReplaysCompletion(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "execute", CommandOptions{Revision: 1})
	calls := 0
	command.Handle(runtime, func(_ Context, input runtimeInput) (runtimeOutput, error) {
		calls++
		return runtimeOutput{Value: input.Value}, nil
	})
	raw, err := json.Marshal(runtimeInput{Value: "result"})
	if err != nil {
		t.Fatal(err)
	}
	job := protocol.JobRequest{JobID: "job", AttemptToken: "attempt", IdempotencyID: "stable", Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: command.Revision(), Mode: command.Mode(), InputSchemaHash: command.Descriptor().InputSchemaHash, OutputSchemaHash: command.Descriptor().OutputSchemaHash, Input: raw, Deadline: time.Now().Add(time.Minute)}
	execution := &executionContext{Context: context.Background(), job: job}
	first, err := runtime.dispatchCommand(execution, job)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.dispatchCommand(execution, job)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || string(first) != string(second) {
		t.Fatalf("calls = %d, outputs = %s / %s", calls, first, second)
	}
}

func TestOverlappingAttemptsAreCancelledAndDeletedIndependently(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "overlap", CommandOptions{Revision: 1, Idempotent: true})
	oldStarted, newStarted, releaseOld := make(chan struct{}), make(chan struct{}), make(chan struct{})
	command.Handle(runtime, func(ctx Context, input runtimeInput) (runtimeOutput, error) {
		if input.Value == "old" {
			close(oldStarted)
			<-releaseOld
			return runtimeOutput{Value: "old"}, nil
		}
		close(newStarted)
		<-ctx.Done()
		return runtimeOutput{}, ctx.Err()
	})
	descriptor := command.Descriptor()
	makeJob := func(attempt, value string) protocol.JobRequest {
		input, _ := json.Marshal(runtimeInput{Value: value})
		return protocol.JobRequest{JobID: "same-job", AttemptToken: attempt, IdempotencyID: attempt, Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: descriptor.Revision, Mode: descriptor.Mode, InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: input, Deadline: time.Now().Add(time.Minute)}
	}
	oldJob, newJob := makeJob("old-attempt", "old"), makeJob("new-attempt", "new")
	oldResult, newResult := make(chan protocol.JobCompletion, 1), make(chan protocol.JobCompletion, 1)
	go func() { oldResult <- runtime.dispatch(context.Background(), nil, oldJob) }()
	<-oldStarted
	go func() { newResult <- runtime.dispatch(context.Background(), nil, newJob) }()
	<-newStarted
	close(releaseOld)
	if completion := <-oldResult; completion.Status != "success" {
		t.Fatalf("old completion = %+v", completion)
	}
	attempts := runtime.activeAttempts()
	if len(attempts) != 1 || attempts[0].AttemptToken != "new-attempt" {
		t.Fatalf("active attempts after old completion = %+v", attempts)
	}
	runtime.cancelJob("same-job", "new-attempt")
	if completion := <-newResult; completion.Status != "canceled" {
		t.Fatalf("new completion = %+v", completion)
	}
}

func TestReconcileIndeterminateJob(t *testing.T) {
	runtime := testRuntime(t.TempDir(), nilBuffer(), nil)
	path := runtime.idempotencyPath("indeterminate")
	if err := runtime.saveIdempotency(path, idempotencyRecord{Version: 1, Status: "indeterminate", JobID: "job", AttemptToken: "attempt", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcileJob(t.Context(), []string{"indeterminate", "--output-json", `{"ok":true}`}); err != nil {
		t.Fatal(err)
	}
	record, found, err := runtime.loadIdempotency(path)
	if err != nil || !found || record.Status != "completed" || string(record.Output) != `{"ok":true}` {
		t.Fatalf("record = %+v, found = %t, error = %v", record, found, err)
	}
}

func nilBuffer() *bytes.Buffer { return &bytes.Buffer{} }
