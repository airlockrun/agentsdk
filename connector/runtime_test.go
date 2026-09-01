package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

type runtimeInput struct {
	Value string `json:"value"`
}

type runtimeOutput struct {
	Value string `json:"value"`
}

func TestHostedRuntimeHandshakeProgressAndCompletion(t *testing.T) {
	settings := DefineSettings[struct {
		Prefix string `json:"prefix" connector:"string,required"`
	}]()
	hostReader, childOutput := io.Pipe()
	childInput, hostWriter := io.Pipe()
	runtime := New(Config{
		Kind: "hosted-test", Contract: DefineContract("io.airlockrun.hosted_test"), Name: "Hosted", Description: "Hosted runtime test.", ArtifactVersion: "1",
		Targets: []string{PlatformLinuxAMD64}, Settings: settings, Input: childInput, Output: childOutput, ErrorOutput: io.Discard,
	})
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "execute", CommandOptions{Revision: 1, Mode: protocol.CommandModeJob})
	command.Handle(runtime, func(ctx Context, input runtimeInput) (runtimeOutput, error) {
		if err := ctx.Progress("working", "started", 1, 2); err != nil {
			return runtimeOutput{}, err
		}
		return runtimeOutput{Value: settings.Get().Prefix + input.Value}, nil
	})
	done := make(chan error, 1)
	go func() { done <- runtime.RunContext(context.Background(), nil) }()
	encoder := protocol.NewChildEncoder(hostWriter)
	decoder := protocol.NewChildDecoder(hostReader)
	stateDirectory := filepath.Join(t.TempDir(), "state")
	initialization := protocol.ChildInitialize{ProtocolVersion: protocol.HostProtocolVersion, InstallationID: "connector-1", StateDirectory: stateDirectory, Settings: json.RawMessage(`{"prefix":"ok:"}`)}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageInitialize, Initialize: &initialization}); err != nil {
		t.Fatal(err)
	}
	var ready protocol.ChildEnvelope
	if err := decoder.Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.Ready.Readiness != protocol.ReadinessReady || ready.Ready.ProtocolVersion != protocol.HostProtocolVersion {
		t.Fatalf("ready = %+v", ready.Ready)
	}
	descriptor := command.Descriptor()
	job := protocol.JobRequest{
		JobID: "job-1", AttemptToken: "attempt-1", IdempotencyID: "stable-1", Kind: protocol.JobKindCommand,
		Operation: command.Name(), Revision: descriptor.Revision, Mode: descriptor.Mode,
		InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash,
		Input: json.RawMessage(`{"value":"done"}`), Deadline: time.Now().Add(time.Minute),
	}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageJob, Job: &job}); err != nil {
		t.Fatal(err)
	}
	var event, completion protocol.ChildEnvelope
	if err := decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&completion); err != nil {
		t.Fatal(err)
	}
	if event.Event == nil || event.Event.Phase != "working" {
		t.Fatalf("event = %+v", event)
	}
	if completion.Completion == nil || completion.Completion.Status != "success" || string(completion.Completion.Output) != `{"value":"ok:done"}` {
		t.Fatalf("completion = %+v", completion)
	}
	if err := hostWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertPanicContains(t, "unavailable during connector definition", func() { settings.Get() })
}

func TestHostedRuntimeCancellation(t *testing.T) {
	hostReader, childOutput := io.Pipe()
	childInput, hostWriter := io.Pipe()
	runtime := New(Config{Kind: "cancel-test", Contract: DefineContract("io.airlockrun.cancel_test"), Name: "Cancel", Description: "Cancellation test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Input: childInput, Output: childOutput, ErrorOutput: io.Discard})
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "wait", CommandOptions{Revision: 1, Mode: protocol.CommandModeJob, Idempotent: true})
	started := make(chan struct{})
	command.Handle(runtime, func(ctx Context, _ runtimeInput) (runtimeOutput, error) {
		close(started)
		<-ctx.Done()
		return runtimeOutput{}, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- runtime.RunContext(context.Background(), nil) }()
	encoder, decoder := protocol.NewChildEncoder(hostWriter), protocol.NewChildDecoder(hostReader)
	initialization := protocol.ChildInitialize{ProtocolVersion: protocol.HostProtocolVersion, InstallationID: "connector-1", StateDirectory: filepath.Join(t.TempDir(), "state"), Settings: json.RawMessage(`{}`)}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageInitialize, Initialize: &initialization}); err != nil {
		t.Fatal(err)
	}
	var envelope protocol.ChildEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	descriptor := command.Descriptor()
	job := protocol.JobRequest{JobID: "job", AttemptToken: "attempt", Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: 1, Mode: descriptor.Mode, InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: json.RawMessage(`{"value":""}`), Deadline: time.Now().Add(time.Minute)}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageJob, Job: &job}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel := protocol.ChildCancel{JobID: job.JobID, AttemptToken: job.AttemptToken}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageCancel, Cancel: &cancel}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Completion == nil || envelope.Completion.Status != "canceled" {
		t.Fatalf("completion = %+v", envelope)
	}
	_ = hostWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHostedRuntimeCancellationWhileWaitingForCapacity(t *testing.T) {
	hostReader, childOutput := io.Pipe()
	childInput, hostWriter := io.Pipe()
	runtime := New(Config{Kind: "queued-cancel-test", Contract: DefineContract("io.airlockrun.queued_cancel_test"), Name: "Queued cancel", Description: "Queued cancellation test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, MaxConcurrency: 1, Input: childInput, Output: childOutput, ErrorOutput: io.Discard})
	command := DefineCommand[runtimeInput, runtimeOutput](runtime.config.Contract, "wait", CommandOptions{Revision: 1, Mode: protocol.CommandModeJob, Idempotent: true})
	started := make(chan struct{})
	command.Handle(runtime, func(ctx Context, _ runtimeInput) (runtimeOutput, error) {
		close(started)
		<-ctx.Done()
		return runtimeOutput{}, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- runtime.RunContext(context.Background(), nil) }()
	encoder, decoder := protocol.NewChildEncoder(hostWriter), protocol.NewChildDecoder(hostReader)
	initialization := protocol.ChildInitialize{ProtocolVersion: protocol.HostProtocolVersion, InstallationID: "connector-1", StateDirectory: filepath.Join(t.TempDir(), "state"), Settings: json.RawMessage(`{}`)}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageInitialize, Initialize: &initialization}); err != nil {
		t.Fatal(err)
	}
	var envelope protocol.ChildEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	descriptor := command.Descriptor()
	job := func(id string) protocol.JobRequest {
		return protocol.JobRequest{JobID: id, AttemptToken: "attempt-" + id, Kind: protocol.JobKindCommand, Operation: command.Name(), Revision: 1, Mode: descriptor.Mode, InputSchemaHash: descriptor.InputSchemaHash, OutputSchemaHash: descriptor.OutputSchemaHash, Input: json.RawMessage(`{"value":""}`), Deadline: time.Now().Add(time.Minute)}
	}
	first, second := job("first"), job("second")
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageJob, Job: &first}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageJob, Job: &second}); err != nil {
		t.Fatal(err)
	}
	cancelSecond := protocol.ChildCancel{JobID: second.JobID, AttemptToken: second.AttemptToken}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageCancel, Cancel: &cancelSecond}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Completion == nil || envelope.Completion.AttemptToken != second.AttemptToken || envelope.Completion.Status != "canceled" {
		t.Fatalf("queued completion = %+v", envelope.Completion)
	}
	cancelFirst := protocol.ChildCancel{JobID: first.JobID, AttemptToken: first.AttemptToken}
	if err := encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageCancel, Cancel: &cancelFirst}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	_ = hostWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRejectsStandaloneLifecycleCommands(t *testing.T) {
	runtime := New(Config{Kind: "only-hosted", Contract: DefineContract("io.airlockrun.only_hosted"), Name: "Hosted", Description: "Hosted only.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}, Input: bytes.NewReader(nil), Output: io.Discard})
	for _, command := range []string{"activate", "install", "configure", "upgrade", "rollback"} {
		t.Run(command, func(t *testing.T) {
			if err := runtime.RunContext(context.Background(), []string{command}); err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestIndeterminateIdempotencyRecordFailsLoud(t *testing.T) {
	runtime := New(Config{Kind: "idempotency", Contract: DefineContract("io.airlockrun.idempotency"), Name: "Idempotency", Description: "Idempotency test.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}})
	runtime.stateDir = t.TempDir()
	job := protocol.JobRequest{JobID: "new-job", AttemptToken: "new-attempt", IdempotencyID: "crashed"}
	path := runtime.idempotencyPath(job.IdempotencyID)
	if err := runtime.saveIdempotency(path, idempotencyRecord{Version: 1, Status: "indeterminate", JobID: "old-job", AttemptToken: "old-attempt", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.executeOnce(context.Background(), job, func() (json.RawMessage, error) { return nil, errors.New("must not run") }); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("error = %v", err)
	}
}
