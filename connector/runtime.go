package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

const (
	defaultCommandInputLimit  = protocol.MaxJobPayloadBytes
	defaultCommandOutputLimit = protocol.MaxJobPayloadBytes
	maxIdempotencyRecords     = 4096
	maxIdempotencyBytes       = 256 << 20
	idempotencyRetention      = 30 * 24 * time.Hour
	idempotencyRecordReserve  = protocol.MaxJobPayloadBytes + (64 << 10)
	maxIdempotencyErrorBytes  = 64 << 10
)

// Config declares a hosted connector child. Runtime transport and persistent
// installation state are supplied by airlock-host during initialization.
type Config struct {
	Kind            string
	Contract        Contract
	Name            string
	Description     string
	ArtifactVersion string
	Targets         []string
	Settings        SettingsDefinition
	Validate        func(context.Context) error
	SelfTest        func(context.Context) error
	MaxConcurrency  int
	Input           io.Reader
	Output          io.Writer
	ErrorOutput     io.Writer
}

type commandRegistration struct {
	descriptor                    protocol.CommandDescriptor
	timeout                       time.Duration
	maxInputBytes, maxOutputBytes int64
	idempotent                    bool
	handle                        func(Context, json.RawMessage) (json.RawMessage, error)
}

type directoryRegistration struct {
	descriptor protocol.DirectoryDescriptor
	provider   *LocalDirectoryProvider
}

type Runtime struct {
	config          Config
	initialSettings []byte
	settings        []protocol.SettingDescriptor
	settingFields   []settingsField
	settingValues   any
	settingHandle   SettingsDefinition
	commands        map[string]commandRegistration
	directories     map[string]directoryRegistration
	definitionMu    sync.Mutex
	frozen          bool
	startHooks      []startHook
	startHookNames  map[string]bool
	executing       atomic.Bool
	stateDir        string
	installationID  string
	storageOrigins  []string
	encoder         *protocol.ChildEncoder
	activeMu        sync.Mutex
	active          map[activeAttemptKey]activeJob
}

func New(config Config) *Runtime {
	if err := protocol.ValidateKind(config.Kind); err != nil {
		panic(err)
	}
	if config.Contract.id == "" {
		panic("connector: Config.Contract is required")
	}
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.Description) == "" || strings.TrimSpace(config.ArtifactVersion) == "" {
		panic("connector: Config.Name, Description, and ArtifactVersion are required")
	}
	if len(config.Targets) == 0 {
		panic("connector: Config.Targets is required")
	}
	settingValues := any(&struct{}{})
	var settings []protocol.SettingDescriptor
	var fields []settingsField
	if config.Settings != nil {
		settingValues, settings, fields = config.Settings.definition()
	}
	initialSettings, err := json.Marshal(settingValues)
	if err != nil {
		panic(err)
	}
	if config.MaxConcurrency == 0 {
		config.MaxConcurrency = 4
	}
	if config.MaxConcurrency < 1 || config.MaxConcurrency > protocol.MaxActiveAttempts {
		panic("connector: MaxConcurrency must be between 1 and 256")
	}
	if config.Input == nil {
		config.Input = os.Stdin
	}
	if config.Output == nil {
		config.Output = os.Stdout
	}
	if config.ErrorOutput == nil {
		config.ErrorOutput = os.Stderr
	}
	config.Targets = append([]string(nil), config.Targets...)
	runtime := &Runtime{
		config: config, initialSettings: initialSettings, settings: settings, settingFields: fields,
		settingValues: settingValues, settingHandle: config.Settings, commands: make(map[string]commandRegistration),
		directories: make(map[string]directoryRegistration), startHookNames: make(map[string]bool), active: make(map[activeAttemptKey]activeJob),
	}
	if config.Settings != nil {
		config.Settings.claim(runtime)
	}
	return runtime
}

func (r *Runtime) freeze() {
	r.definitionMu.Lock()
	defer r.definitionMu.Unlock()
	r.frozen = true
}

func (r *Runtime) registerCommand(command commandRegistration) {
	r.definitionMu.Lock()
	defer r.definitionMu.Unlock()
	if r.frozen {
		panic("connector: registrations are frozen after Manifest or Run")
	}
	if command.descriptor.Name == "" || command.handle == nil {
		panic("connector: invalid command registration")
	}
	if _, exists := r.commands[command.descriptor.Name]; exists {
		panic("connector: duplicate command " + command.descriptor.Name)
	}
	if command.maxInputBytes == 0 {
		command.maxInputBytes = defaultCommandInputLimit
	}
	if command.maxOutputBytes == 0 {
		command.maxOutputBytes = defaultCommandOutputLimit
	}
	if command.maxInputBytes > protocol.MaxJobPayloadBytes || command.maxOutputBytes > protocol.MaxJobPayloadBytes {
		panic(fmt.Sprintf("connector: command payload limits cannot exceed %d bytes", protocol.MaxJobPayloadBytes))
	}
	if command.timeout == 0 {
		if command.descriptor.Mode == protocol.CommandModeJob {
			command.timeout = 24 * time.Hour
		} else {
			command.timeout = 30 * time.Second
		}
	}
	r.commands[command.descriptor.Name] = command
}

func (r *Runtime) registerDirectory(directory directoryRegistration) {
	r.definitionMu.Lock()
	defer r.definitionMu.Unlock()
	if r.frozen {
		panic("connector: registrations are frozen after Manifest or Run")
	}
	if directory.provider.binding != nil && directory.provider.binding.settings != r.settingHandle {
		panic("connector: bound local directory belongs to different settings")
	}
	if _, exists := r.directories[directory.descriptor.Name]; exists {
		panic("connector: duplicate directory " + directory.descriptor.Name)
	}
	r.directories[directory.descriptor.Name] = directory
}

func (r *Runtime) Manifest() protocol.Manifest {
	r.freeze()
	commands := make([]protocol.CommandDescriptor, 0, len(r.commands))
	for _, name := range sortedMapKeys(r.commands) {
		commands = append(commands, cloneCommandDescriptor(r.commands[name].descriptor))
	}
	directories := make([]protocol.DirectoryDescriptor, 0, len(r.directories))
	for _, name := range sortedMapKeys(r.directories) {
		directories = append(directories, r.directories[name].descriptor)
	}
	targets := append([]string(nil), r.config.Targets...)
	sort.Strings(targets)
	settings := make([]protocol.SettingDescriptor, len(r.settings))
	for i, setting := range r.settings {
		settings[i] = setting
		settings[i].Enum = append([]string(nil), setting.Enum...)
	}
	sort.Slice(settings, func(i, j int) bool { return settings[i].Name < settings[j].Name })
	iface := protocol.Interface{Kind: r.config.Kind, ContractID: r.config.Contract.id, Name: r.config.Name, Description: r.config.Description, ArtifactVersion: r.config.ArtifactVersion, Commands: commands, Directories: directories}
	interfaceHash, err := protocol.InterfaceDigest(iface)
	if err != nil {
		panic("connector: interface digest: " + err.Error())
	}
	artifactDigest, err := executableDigest()
	if err != nil {
		panic("connector: executable digest: " + err.Error())
	}
	manifest := protocol.Manifest{
		ProtocolMajor: protocol.Major, ProtocolMinor: protocol.Minor,
		Features: []string{"cancellation", "commands", "directories", "hosted-child-v1", "long-jobs"},
		Targets:  targets, Interface: iface, InterfaceHash: interfaceHash, ArtifactDigest: artifactDigest, Settings: settings,
	}
	if err := protocol.ValidateManifest(manifest); err != nil {
		panic(err)
	}
	return manifest
}

func (r *Runtime) Run() error { return r.RunContext(context.Background(), os.Args[1:]) }

// RunContext runs manifest/version/validate inspection or the hosted child
// protocol. The run command and an empty argument list both select child mode.
func (r *Runtime) RunContext(ctx context.Context, args []string) error {
	if !r.executing.CompareAndSwap(false, true) {
		return errors.New("connector: Runtime.RunContext cannot execute concurrently")
	}
	defer r.executing.Store(false)
	if mode := os.Getenv("AIRLOCK_CONNECTOR_MODE"); mode != "" {
		if mode != "manifest" || len(args) != 0 {
			return fmt.Errorf("connector: unsupported AIRLOCK_CONNECTOR_MODE %q", mode)
		}
		args = []string{"manifest"}
	}
	if len(args) > 1 {
		return errors.New("connector: expected one of run, manifest, version, or validate")
	}
	command := "run"
	if len(args) == 1 {
		command = args[0]
	}
	switch command {
	case "manifest":
		return json.NewEncoder(r.config.Output).Encode(r.Manifest())
	case "version":
		_, err := fmt.Fprintf(r.config.Output, "%s %s protocol %d.%d host-child %d\n", r.config.Kind, r.config.ArtifactVersion, protocol.Major, protocol.Minor, protocol.HostProtocolVersion)
		return err
	case "validate":
		r.freeze()
		if err := r.applySettings(ctx, json.RawMessage(r.initialSettings), nil); err != nil {
			return err
		}
		defer r.shutdown()
		return nil
	case "run":
		return r.runHosted(ctx)
	default:
		return fmt.Errorf("connector: unknown command %q", command)
	}
}

func (r *Runtime) runHosted(ctx context.Context) error {
	r.freeze()
	r.encoder = protocol.NewChildEncoder(r.config.Output)
	decoder := protocol.NewChildDecoder(r.config.Input)
	var first protocol.ChildEnvelope
	if err := decoder.Decode(&first); err != nil {
		return fmt.Errorf("connector: read initialization: %w", err)
	}
	if first.Type != protocol.ChildMessageInitialize {
		return errors.New("connector: first child message must initialize the runtime")
	}
	init := first.Initialize
	if init.ProtocolVersion != protocol.HostProtocolVersion {
		return fmt.Errorf("connector: unsupported host protocol version %d", init.ProtocolVersion)
	}
	if strings.TrimSpace(init.InstallationID) == "" || !filepath.IsAbs(init.StateDirectory) {
		return errors.New("connector: initialization requires an installation ID and absolute state directory")
	}
	if err := ensurePrivateDirectory(init.StateDirectory); err != nil {
		return fmt.Errorf("connector: prepare child state directory: %w", err)
	}
	r.installationID, r.stateDir = init.InstallationID, init.StateDirectory
	if err := r.applySettings(ctx, init.Settings, init.StorageOrigins); err != nil {
		_ = r.sendReady(protocol.ReadinessUnhealthy, err)
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		r.cancelAll()
		workers.Wait()
		r.shutdown()
	}()
	if err := r.runStartHooks(runCtx); err != nil {
		err = fmt.Errorf("connector: start: %w", err)
		_ = r.sendReady(protocol.ReadinessUnhealthy, err)
		return err
	}
	if err := r.sendReady(protocol.ReadinessReady, nil); err != nil {
		return err
	}
	semaphore := make(chan struct{}, r.config.MaxConcurrency)
	for {
		var envelope protocol.ChildEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("connector: decode host message: %w", err)
		}
		switch envelope.Type {
		case protocol.ChildMessageJob:
			job := *envelope.Job
			dispatchCtx, dispatchCancel, key, err := r.prepareDispatch(runCtx, job)
			if err != nil {
				completion := protocol.JobCompletion{AttemptToken: job.AttemptToken, Status: "error", Error: err.Error()}
				if err := r.encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageCompletion, Completion: &completion}); err != nil {
					_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
				}
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				completion := r.dispatch(dispatchCtx, dispatchCancel, key, job, semaphore)
				if err := r.encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageCompletion, Completion: &completion}); err != nil {
					_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
				}
			}()
		case protocol.ChildMessageCancel:
			r.cancelJob(envelope.Cancel.JobID, envelope.Cancel.AttemptToken)
		case protocol.ChildMessageSettings:
			if r.activeCount() != 0 {
				_ = r.sendReady(protocol.ReadinessUnhealthy, errors.New("connector: settings cannot change while jobs are active"))
				continue
			}
			if err := r.applySettings(runCtx, envelope.Settings.Settings, envelope.Settings.StorageOrigins); err != nil {
				_ = r.sendReady(protocol.ReadinessUnhealthy, err)
				continue
			}
			if err := r.sendReady(protocol.ReadinessReady, nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("connector: host sent invalid message type %q", envelope.Type)
		}
	}
}

func (r *Runtime) sendReady(readiness protocol.Readiness, readyErr error) error {
	ready := protocol.ChildReady{ProtocolVersion: protocol.HostProtocolVersion, Manifest: r.Manifest(), Readiness: readiness}
	if readyErr != nil {
		ready.Error = boundedError(readyErr)
	}
	return r.encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageReady, Ready: &ready})
}

func (r *Runtime) applySettings(ctx context.Context, raw json.RawMessage, origins []string) error {
	if len(raw) == 0 || len(raw) > protocol.MaxJobPayloadBytes || !json.Valid(raw) {
		return errors.New("connector: settings must be bounded valid JSON")
	}
	target := reflect.New(reflect.ValueOf(r.settingValues).Elem().Type())
	if err := json.Unmarshal(r.initialSettings, target.Interface()); err != nil {
		return err
	}
	if err := strictUnmarshal(raw, target.Interface()); err != nil {
		return fmt.Errorf("connector: decode host settings: %w", err)
	}
	var supplied map[string]json.RawMessage
	if err := json.Unmarshal(raw, &supplied); err != nil || supplied == nil {
		return errors.New("connector: host settings must be a JSON object")
	}
	for _, field := range r.settingFields {
		value := target.Elem().Field(field.index)
		rawValue, provided := supplied[field.jsonName]
		if provided && bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return fmt.Errorf("connector: setting %s cannot be null", field.name)
		}
		if provided || !value.IsZero() {
			if err := validateSettingValue(value, field); err != nil {
				return fmt.Errorf("connector: setting %s: %w", field.name, err)
			}
		}
		if field.required && value.IsZero() {
			return fmt.Errorf("connector: required setting %s is missing", field.name)
		}
	}
	validatedOrigins, err := validateStorageOrigins(origins)
	if err != nil {
		return err
	}
	previous := reflect.New(reflect.ValueOf(r.settingValues).Elem().Type())
	previous.Elem().Set(reflect.ValueOf(r.settingValues).Elem())
	previousOrigins := append([]string(nil), r.storageOrigins...)
	restore := func() {
		reflect.ValueOf(r.settingValues).Elem().Set(previous.Elem())
		if r.settingHandle != nil {
			r.settingHandle.publish()
		}
		_ = r.directoryRootBindings().apply(r.settingValues)
		for _, directory := range r.directories {
			directory.provider.setOrigins(previousOrigins)
		}
		r.storageOrigins = previousOrigins
	}
	reflect.ValueOf(r.settingValues).Elem().Set(target.Elem())
	if r.settingHandle != nil {
		r.settingHandle.publish()
	}
	if err := r.directoryRootBindings().apply(r.settingValues); err != nil {
		restore()
		return err
	}
	for _, directory := range r.directories {
		directory.provider.setOrigins(validatedOrigins)
	}
	r.storageOrigins = validatedOrigins
	if err := r.validate(ctx); err != nil {
		restore()
		return err
	}
	return nil
}

func (r *Runtime) validate(ctx context.Context) error {
	if r.config.Validate != nil {
		if err := r.config.Validate(ctx); err != nil {
			return fmt.Errorf("connector: validate settings: %w", err)
		}
	}
	if r.config.SelfTest != nil {
		if err := r.config.SelfTest(ctx); err != nil {
			return fmt.Errorf("connector: self-test: %w", err)
		}
	}
	return nil
}

func (r *Runtime) shutdown() {
	for _, directory := range r.directories {
		_ = directory.provider.Close()
	}
	if r.settingHandle != nil {
		r.settingHandle.clear()
	}
}

type directoryRootBinding struct {
	provider *LocalDirectoryProvider
	field    int
}

type directoryRootBindings []directoryRootBinding

func (r *Runtime) directoryRootBindings() directoryRootBindings {
	bindings := make(directoryRootBindings, 0, len(r.directories))
	for _, directory := range r.directories {
		if directory.provider.binding != nil {
			bindings = append(bindings, directoryRootBinding{provider: directory.provider, field: directory.provider.binding.field.index})
		}
	}
	return bindings
}

func (bindings directoryRootBindings) apply(settings any) error {
	value := reflect.ValueOf(settings).Elem()
	for _, binding := range bindings {
		if err := binding.provider.rebind(value.Field(binding.field).String()); err != nil {
			return fmt.Errorf("connector: bind local directory setting: %w", err)
		}
	}
	return nil
}

func validateStorageOrigins(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		request, err := http.NewRequest(http.MethodGet, value, nil)
		if err != nil || request.URL.Scheme != "https" || request.URL.Host == "" || request.URL.User != nil || request.URL.Path != "" || request.URL.RawQuery != "" || request.URL.Fragment != "" {
			return nil, fmt.Errorf("connector: invalid host-approved storage origin %q", value)
		}
		origin := "https://" + strings.ToLower(request.URL.Host)
		if !seen[origin] {
			seen[origin] = true
			result = append(result, origin)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (r *Runtime) prepareDispatch(parent context.Context, job protocol.JobRequest) (context.Context, context.CancelFunc, activeAttemptKey, error) {
	if job.JobID == "" || job.AttemptToken == "" || job.Deadline.IsZero() || !job.Deadline.After(time.Now()) {
		return nil, nil, activeAttemptKey{}, errors.New("invalid or expired job delivery")
	}
	if len(job.Input) == 0 || len(job.Input) > protocol.MaxJobPayloadBytes || !json.Valid(job.Input) {
		return nil, nil, activeAttemptKey{}, errors.New("invalid or oversized job input")
	}
	ctx, cancel := context.WithDeadline(parent, job.Deadline)
	key := activeAttemptKey{jobID: job.JobID, attemptToken: job.AttemptToken}
	r.activeMu.Lock()
	if _, exists := r.active[key]; exists {
		r.activeMu.Unlock()
		cancel()
		return nil, nil, activeAttemptKey{}, errors.New("duplicate active job attempt")
	}
	r.active[key] = activeJob{cancel: cancel}
	r.activeMu.Unlock()
	return ctx, cancel, key, nil
}

func (r *Runtime) dispatch(ctx context.Context, cancel context.CancelFunc, key activeAttemptKey, job protocol.JobRequest, semaphore chan struct{}) protocol.JobCompletion {
	completion := protocol.JobCompletion{AttemptToken: job.AttemptToken, Status: "error"}
	defer func() {
		cancel()
		r.activeMu.Lock()
		delete(r.active, key)
		r.activeMu.Unlock()
	}()
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			completion.Status = "timeout"
		} else {
			completion.Status = "canceled"
		}
		completion.Error = boundedError(ctx.Err())
		return completion
	}
	execution := &executionContext{Context: ctx, job: job, send: r.sendEvent}
	var output json.RawMessage
	var err error
	if job.Kind == protocol.JobKindCommand {
		output, err = r.dispatchCommand(execution, job)
	} else {
		output, err = r.dispatchDirectory(execution, job)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			completion.Status = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			completion.Status = "timeout"
		}
		completion.Error = boundedError(err)
		return completion
	}
	completion.Status, completion.Output = "success", output
	return completion
}

func (r *Runtime) sendEvent(event protocol.JobEvent) error {
	return r.encoder.Encode(protocol.ChildEnvelope{Type: protocol.ChildMessageEvent, Event: &event})
}

func (r *Runtime) dispatchCommand(ctx *executionContext, job protocol.JobRequest) (json.RawMessage, error) {
	command, exists := r.commands[job.Operation]
	if !exists {
		return nil, fmt.Errorf("connector: unknown command %q", job.Operation)
	}
	if job.Revision != command.descriptor.Revision || job.Mode != command.descriptor.Mode || job.InputSchemaHash != command.descriptor.InputSchemaHash || job.OutputSchemaHash != command.descriptor.OutputSchemaHash {
		return nil, errors.New("connector: command descriptor mismatch")
	}
	if int64(len(job.Input)) > command.maxInputBytes {
		return nil, errors.New("connector: command input exceeds limit")
	}
	commandCtx, cancel := context.WithTimeout(ctx.Context, command.timeout)
	defer cancel()
	ctx.Context, ctx.mode = commandCtx, command.descriptor.Mode
	execute := func() (json.RawMessage, error) {
		output, err := command.handle(ctx, job.Input)
		if err != nil {
			return nil, err
		}
		if err := commandCtx.Err(); err != nil {
			return nil, err
		}
		if int64(len(output)) > command.maxOutputBytes {
			return nil, errors.New("connector: command output exceeds limit")
		}
		return output, nil
	}
	if command.idempotent {
		return execute()
	}
	return r.executeOnce(ctx, job, execute)
}

func (r *Runtime) dispatchDirectory(ctx *executionContext, job protocol.JobRequest) (json.RawMessage, error) {
	directory, exists := r.directories[job.Operation]
	if !exists {
		return nil, fmt.Errorf("connector: unknown directory %q", job.Operation)
	}
	if job.Revision != directory.descriptor.Revision {
		return nil, errors.New("connector: directory revision mismatch")
	}
	mutating := job.Kind == protocol.JobKindDirectoryWrite || job.Kind == protocol.JobKindDirectoryDelete || job.Kind == protocol.JobKindDirectoryMove || job.Kind == protocol.JobKindDirectoryImport || job.Kind == protocol.JobKindDirectoryExport
	if mutating {
		return r.executeOnce(ctx, job, func() (json.RawMessage, error) { return r.dispatchDirectoryOperation(ctx, directory, job) })
	}
	return r.dispatchDirectoryOperation(ctx, directory, job)
}

func (r *Runtime) dispatchDirectoryOperation(ctx *executionContext, directory directoryRegistration, job protocol.JobRequest) (json.RawMessage, error) {
	var result any
	switch job.Kind {
	case protocol.JobKindDirectoryList:
		if !directory.descriptor.List {
			return nil, errors.New("connector: directory does not allow list")
		}
		var request protocol.DirectoryListRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.list(ctx, request.Path, request.Cursor, request.Limit)
		if err != nil {
			return nil, err
		}
		result = value
	case protocol.JobKindDirectoryStat:
		if !directory.descriptor.Read {
			return nil, errors.New("connector: directory does not allow read")
		}
		var request struct {
			Path string `json:"path"`
		}
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.Stat(request.Path)
		if err != nil {
			return nil, err
		}
		result = value
	case protocol.JobKindDirectoryRead:
		if !directory.descriptor.Read {
			return nil, errors.New("connector: directory does not allow read")
		}
		var request protocol.DirectoryReadRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.Read(request.Path, request.Offset, request.Length)
		if err != nil {
			return nil, err
		}
		result = value
	case protocol.JobKindDirectoryWrite:
		if !directory.descriptor.Write {
			return nil, errors.New("connector: directory does not allow write")
		}
		var request protocol.DirectoryWriteRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.Write(request.Path, request.Data, request.Overwrite)
		if err != nil {
			return nil, err
		}
		result = value
	case protocol.JobKindDirectoryDelete:
		if !directory.descriptor.Write {
			return nil, errors.New("connector: directory does not allow write")
		}
		var request struct {
			Path string `json:"path"`
		}
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		if err := directory.provider.Delete(request.Path); err != nil {
			return nil, err
		}
		result = struct{}{}
	case protocol.JobKindDirectoryMove:
		if !directory.descriptor.Write {
			return nil, errors.New("connector: directory does not allow write")
		}
		var request protocol.DirectoryMoveRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		if err := directory.provider.Move(request.From, request.To, request.Overwrite); err != nil {
			return nil, err
		}
		result = struct{}{}
	case protocol.JobKindDirectoryImport:
		if !directory.descriptor.Write {
			return nil, errors.New("connector: directory does not allow write")
		}
		var request protocol.DirectoryImportRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.Import(ctx, request)
		if err != nil {
			return nil, err
		}
		result = value
	case protocol.JobKindDirectoryExport:
		if !directory.descriptor.Read {
			return nil, errors.New("connector: directory does not allow read")
		}
		var request protocol.DirectoryExportRequest
		if err := strictUnmarshal(job.Input, &request); err != nil {
			return nil, err
		}
		value, err := directory.provider.Export(ctx, request)
		if err != nil {
			return nil, err
		}
		result = value
	default:
		return nil, fmt.Errorf("connector: unsupported directory operation %q", job.Kind)
	}
	return json.Marshal(result)
}

type activeAttemptKey struct{ jobID, attemptToken string }
type activeJob struct{ cancel context.CancelFunc }

func (r *Runtime) cancelJob(id, attemptToken string) {
	r.activeMu.Lock()
	active := r.active[activeAttemptKey{jobID: id, attemptToken: attemptToken}]
	r.activeMu.Unlock()
	if active.cancel != nil {
		active.cancel()
	}
}

func (r *Runtime) cancelAll() {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	for _, active := range r.active {
		active.cancel()
	}
}

func (r *Runtime) activeCount() int {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	return len(r.active)
}

type executionContext struct {
	context.Context
	job      protocol.JobRequest
	send     func(protocol.JobEvent) error
	sequence int64
	mode     protocol.CommandMode
}

func (c *executionContext) JobID() string         { return c.job.JobID }
func (c *executionContext) IdempotencyID() string { return c.job.IdempotencyID }
func (c *executionContext) Progress(phase, message string, completed, total int64) error {
	if c.mode != protocol.CommandModeJob {
		return errors.New("connector: progress is available only for job-mode commands")
	}
	if completed < 0 || total < 0 || total > 0 && completed > total {
		return errors.New("connector: invalid progress")
	}
	c.sequence++
	return c.send(protocol.JobEvent{AttemptToken: c.job.AttemptToken, Sequence: c.sequence, Phase: phase, Message: message, Completed: completed, Total: total, Time: time.Now().UTC()})
}

type idempotencyRecord struct {
	Version       int             `json:"version"`
	Status        string          `json:"status"`
	JobID         string          `json:"jobId"`
	AttemptToken  string          `json:"attemptToken"`
	Output        json.RawMessage `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	ReservedBytes int64           `json:"reservedBytes,omitempty"`
}

func (r *Runtime) idempotencyPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(r.stateDir, "jobs", hex.EncodeToString(sum[:])+".json")
}
func (r *Runtime) loadIdempotency(path string) (idempotencyRecord, bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return idempotencyRecord{}, false, nil
	}
	if err != nil {
		return idempotencyRecord{}, false, err
	}
	var value idempotencyRecord
	if err := strictUnmarshal(body, &value); err != nil {
		return value, false, err
	}
	return value, true, nil
}
func (r *Runtime) saveIdempotency(path string, value idempotencyRecord) error {
	value.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, 0o600)
}

func (r *Runtime) compactIdempotency(ctx context.Context, now time.Time) (int, int64, error) {
	dir := filepath.Join(r.stateDir, "jobs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	count, total := 0, int64(0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return 0, 0, err
		}
		record, found, err := r.loadIdempotency(path)
		if err != nil || !found {
			return 0, 0, errors.Join(fmt.Errorf("connector: inspect idempotency record %s", entry.Name()), err)
		}
		size, updated := max(info.Size(), record.ReservedBytes), record.UpdatedAt
		if updated.IsZero() {
			updated = record.CreatedAt
		}
		if record.Status != "indeterminate" && !updated.IsZero() && now.Sub(updated) >= idempotencyRetention {
			if err := removeIdempotencyRecord(ctx, path); err != nil {
				return 0, 0, err
			}
			continue
		}
		count++
		total += size
	}
	return count, total, nil
}

func removeIdempotencyRecord(ctx context.Context, path string) error {
	lockPath := path + ".lock"
	lock, err := acquireFileLock(ctx, lockPath)
	if err != nil {
		return err
	}
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	closeErr := lock.Close()
	lockErr := os.Remove(lockPath)
	if errors.Is(lockErr, os.ErrNotExist) {
		lockErr = nil
	}
	return errors.Join(removeErr, closeErr, lockErr)
}

func (r *Runtime) executeOnce(ctx context.Context, job protocol.JobRequest, execute func() (json.RawMessage, error)) (json.RawMessage, error) {
	if job.IdempotencyID == "" || job.AttemptToken == "" {
		return nil, errors.New("connector: mutating job requires idempotency and attempt tokens")
	}
	recordPath := r.idempotencyPath(job.IdempotencyID)
	admission, err := acquireFileLock(ctx, filepath.Join(r.stateDir, "jobs", ".admission.lock"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if admission != nil {
			_ = admission.Close()
		}
	}()
	record, found, err := r.loadIdempotency(recordPath)
	if err != nil {
		return nil, err
	}
	if found {
		if err := admission.Close(); err != nil {
			return nil, err
		}
		admission = nil
		lock, err := acquireFileLock(ctx, recordPath+".lock")
		if err != nil {
			return nil, err
		}
		defer lock.Close()
		record, found, err = r.loadIdempotency(recordPath)
		if err != nil || !found {
			return nil, errors.Join(errors.New("connector: idempotency record disappeared while waiting for its lock"), err)
		}
		return replayIdempotency(job.IdempotencyID, record)
	}
	count, used, err := r.compactIdempotency(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if count >= maxIdempotencyRecords || used+idempotencyRecordReserve > maxIdempotencyBytes {
		return nil, errors.New("connector: durable idempotency storage is full; reconcile indeterminate records or wait for terminal-record retention")
	}
	lock, err := acquireFileLock(ctx, recordPath+".lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if _, found, err := r.loadIdempotency(recordPath); err != nil || found {
		return nil, errors.Join(errors.New("connector: idempotency record appeared during serialized admission"), err)
	}
	record = idempotencyRecord{Version: 1, Status: "indeterminate", JobID: job.JobID, AttemptToken: job.AttemptToken, CreatedAt: time.Now().UTC(), ReservedBytes: idempotencyRecordReserve}
	if err := r.saveIdempotency(recordPath, record); err != nil {
		return nil, err
	}
	if err := admission.Close(); err != nil {
		return nil, err
	}
	admission = nil
	output, executeErr := execute()
	if executeErr != nil {
		record.Status, record.Error, record.ReservedBytes = "failed", boundedError(executeErr), 0
		if err := r.saveIdempotency(recordPath, record); err != nil {
			return nil, errors.Join(executeErr, err)
		}
		return nil, executeErr
	}
	record.Status, record.Output, record.ReservedBytes = "completed", output, 0
	if err := r.saveIdempotency(recordPath, record); err != nil {
		return nil, err
	}
	return output, nil
}

func replayIdempotency(id string, record idempotencyRecord) (json.RawMessage, error) {
	switch record.Status {
	case "completed":
		return record.Output, nil
	case "failed":
		return nil, errors.New(record.Error)
	case "indeterminate":
		return nil, fmt.Errorf("connector: idempotency %s has an indeterminate crash outcome from attempt %s; reconcile locally before retrying", id, record.AttemptToken)
	default:
		return nil, fmt.Errorf("connector: idempotency record has invalid status %q", record.Status)
	}
}

func boundedError(err error) string {
	message := err.Error()
	if len(message) > maxIdempotencyErrorBytes {
		return message[:maxIdempotencyErrorBytes]
	}
	return message
}

func strictUnmarshal(body []byte, result any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("connector: multiple JSON values")
		}
		return err
	}
	return nil
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func executableDigest() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
