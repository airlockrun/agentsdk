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
	"net/url"
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
	maximumProtocolBody       = protocol.MaxEnvelopeBytes
	maxIdempotencyRecords     = 4096
	maxIdempotencyBytes       = 256 << 20
	idempotencyRetention      = 30 * 24 * time.Hour
	idempotencyRecordReserve  = protocol.MaxJobPayloadBytes + (64 << 10)
	maxIdempotencyErrorBytes  = 64 << 10
)

type Config struct {
	Kind              string
	Contract          Contract
	Name              string
	Description       string
	ArtifactVersion   string
	Targets           []string
	Settings          SettingsDefinition
	Validate          func(context.Context) error
	SelfTest          func(context.Context) error
	StateDirectory    string
	InstallationID    string
	HTTPClient        *http.Client
	Operations        Operations
	ServiceMode       ServiceMode
	HeartbeatInterval time.Duration
	MaxConcurrency    int
	AllowInsecureHTTP bool
	Input             io.Reader
	Output            io.Writer
	ErrorOutput       io.Writer
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
	config                 Config
	stateDir               string
	stateBase              string
	installationID         string
	artifactDigest         string
	artifactMu             sync.Mutex
	ambiguousInstallations bool
	initialSettings        []byte
	machineState           bool
	settings               []protocol.SettingDescriptor
	settingFields          []settingsField
	settingValues          any
	settingHandle          SettingsDefinition
	commands               map[string]commandRegistration
	directories            map[string]directoryRegistration
	definitionMu           sync.Mutex
	frozen                 bool
	startHooks             []startHook
	startHookNames         map[string]bool
	executing              atomic.Bool
	activeMu               sync.Mutex
	active                 map[activeAttemptKey]activeJob
	healthy                atomic.Bool
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
	var settingHandle SettingsDefinition
	var settingValues any = &struct{}{}
	var settings []protocol.SettingDescriptor
	var fields []settingsField
	if config.Settings != nil {
		settingHandle = config.Settings
	}
	if settingHandle != nil {
		settingValues, settings, fields = settingHandle.definition()
	}
	if config.ServiceMode != ServiceSystem && config.ServiceMode != ServiceUser {
		panic("connector: Config.ServiceMode must explicitly select connector.ServiceUser or connector.ServiceSystem")
	}
	if config.Operations == nil {
		config.Operations = systemOperations{}
	}
	if config.InstallationID != "" && !validInstallationID(config.InstallationID) {
		panic("connector: InstallationID must be a lowercase UUID")
	}
	initialSettings, err := json.Marshal(settingValues)
	if err != nil {
		panic(err)
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 30 * time.Second
	}
	if config.MaxConcurrency == 0 {
		config.MaxConcurrency = 4
	}
	if config.HeartbeatInterval < time.Second || config.MaxConcurrency < 1 || config.MaxConcurrency > protocol.MaxActiveAttempts {
		panic("connector: heartbeat interval must be at least one second and max concurrency must be between 1 and 256")
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
		config: config, initialSettings: initialSettings, machineState: config.ServiceMode == ServiceSystem, settings: settings, settingFields: fields, settingValues: settingValues, settingHandle: settingHandle,
		commands: make(map[string]commandRegistration), directories: make(map[string]directoryRegistration),
		startHookNames: make(map[string]bool), active: make(map[activeAttemptKey]activeJob),
	}
	if settingHandle != nil {
		settingHandle.claim(runtime)
	}
	return runtime
}

func (r *Runtime) freeze() {
	r.definitionMu.Lock()
	defer r.definitionMu.Unlock()
	r.frozen = true
}

func (r *Runtime) initialize(args []string) error {
	stateBase := r.config.StateDirectory
	if stateBase == "" {
		var err error
		stateBase, err = defaultStateDir(r.config.Kind, r.config.ServiceMode)
		if err != nil {
			return fmt.Errorf("connector: state directory: %w", err)
		}
	}
	if err := prepareStateDirectory(stateBase, r.config.ServiceMode, r.config.Operations); err != nil {
		return fmt.Errorf("connector: prepare state directory: %w", err)
	}
	r.stateBase = stateBase
	r.stateDir = draftStateDirectory(stateBase)
	r.installationID = ""
	r.ambiguousInstallations = false

	selector := r.config.InstallationID
	if selector == "" {
		selector = os.Getenv("AIRLOCK_CONNECTOR_INSTALLATION_ID")
	}
	argumentSelector := installationArgument(args)
	if selector != "" && argumentSelector != "" && selector != argumentSelector {
		return errors.New("connector: --installation conflicts with Config.InstallationID or AIRLOCK_CONNECTOR_INSTALLATION_ID")
	}
	if selector == "" {
		selector = argumentSelector
	}
	if selector != "" && !validInstallationID(selector) {
		return errors.New("connector: installation selector must be a lowercase UUID")
	}
	ids, err := installationDirectories(stateBase)
	if err != nil {
		return err
	}
	if selector == "" && len(ids) == 1 {
		selector = ids[0]
	}
	if selector == "" && len(ids) > 1 {
		r.ambiguousInstallations = true
	}
	if selector != "" {
		found := false
		for _, id := range ids {
			found = found || id == selector
		}
		if !found {
			return fmt.Errorf("connector: selected installation does not exist: %s", selector)
		}
		r.installationID = selector
		r.stateDir = filepath.Join(stateBase, "installations", selector)
	} else if err := prepareStateDirectory(r.stateDir, r.config.ServiceMode, r.config.Operations); err != nil {
		return fmt.Errorf("connector: prepare draft state directory: %w", err)
	}
	if r.settingValues != nil {
		if err := json.Unmarshal(r.initialSettings, r.settingValues); err != nil {
			return err
		}
	}
	if _, err = r.artifactDigestValue(); err != nil {
		return fmt.Errorf("connector: digest running executable: %w", err)
	}
	return nil
}

func (r *Runtime) artifactDigestValue() (string, error) {
	r.artifactMu.Lock()
	defer r.artifactMu.Unlock()
	if r.artifactDigest != "" {
		return r.artifactDigest, nil
	}
	digest, err := executableDigest(r.config.Operations)
	if err != nil {
		return "", err
	}
	r.artifactDigest = digest
	return digest, nil
}

func (r *Runtime) publishSettings() error {
	if r.settingHandle != nil {
		r.settingHandle.publish()
	}
	return r.directoryRootBindings().apply(r.settingValues)
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
	if command.maxInputBytes > protocol.MaxJobPayloadBytes || command.maxOutputBytes > protocol.MaxJobPayloadBytes {
		panic(fmt.Sprintf("connector: command payload limits cannot exceed %d bytes", protocol.MaxJobPayloadBytes))
	}
	if command.maxOutputBytes == 0 {
		command.maxOutputBytes = defaultCommandOutputLimit
	}
	if command.timeout == 0 {
		if command.descriptor.Mode == protocol.CommandModeJob {
			command.timeout = 24 * time.Hour
		} else {
			command.timeout = 30 * time.Second
		}
	}
	if command.descriptor.InputSchemaHash == "" || command.descriptor.OutputSchemaHash == "" {
		panic("connector: command schemas are required")
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
	iface := protocol.Interface{
		Kind: r.config.Kind, ContractID: r.config.Contract.id, Name: r.config.Name,
		Description: r.config.Description, ArtifactVersion: r.config.ArtifactVersion,
		Commands: commands, Directories: directories,
	}
	digest, err := protocol.InterfaceDigest(iface)
	if err != nil {
		panic("connector: interface digest: " + err.Error())
	}
	artifactDigest, err := r.artifactDigestValue()
	if err != nil {
		panic("connector: digest running executable: " + err.Error())
	}
	manifest := protocol.Manifest{
		ProtocolMajor: protocol.Major, ProtocolMinor: protocol.Minor,
		Features: []string{"cancellation", "commands", "directories", "long-jobs"},
		Targets:  targets, ServiceMode: string(r.config.ServiceMode), Interface: iface,
		InterfaceHash: digest, ArtifactDigest: artifactDigest,
		Settings: settings,
	}
	if err := protocol.ValidateManifest(manifest); err != nil {
		panic(err)
	}
	return manifest
}

func (r *Runtime) Run() error { return r.RunContext(context.Background(), os.Args[1:]) }

func (r *Runtime) RunContext(ctx context.Context, args []string) error {
	if !r.executing.CompareAndSwap(false, true) {
		return errors.New("connector: Runtime.RunContext cannot execute concurrently")
	}
	defer r.executing.Store(false)
	if mode := os.Getenv("AIRLOCK_CONNECTOR_MODE"); mode != "" {
		if mode != "manifest" {
			return fmt.Errorf("connector: unsupported AIRLOCK_CONNECTOR_MODE %q", mode)
		}
		if len(args) != 0 {
			return errors.New("connector: manifest mode takes no arguments")
		}
		encoded, err := json.Marshal(r.Manifest())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(r.config.Output, string(encoded))
		return err
	}
	if len(args) == 0 {
		return errors.New("connector: command is required (activate, run, install, uninstall, start, stop, restart, status, configure, validate, version, unregister, upgrade, rollback, enable, disable)")
	}
	r.freeze()
	if err := r.initialize(args); err != nil {
		return err
	}
	if r.settingHandle != nil {
		defer r.settingHandle.clear()
	}
	var err error
	args, err = r.extractInstallation(args)
	if err != nil {
		return err
	}
	unselectedCommand := args[0] == "activate" && (containsArgument(args[1:], "--new") || containsArgument(args[1:], "--check")) || args[0] == "validate-service" && containsArgument(args[1:], "--draft")
	if r.ambiguousInstallations && !unselectedCommand {
		return errors.New("connector: multiple installations exist; select one with --installation or AIRLOCK_CONNECTOR_INSTALLATION_ID")
	}
	if args[0] != "activate" && installationCommandNeedsLock(args[0]) {
		lock, err := acquireFileLock(ctx, r.installationLockPath())
		if err != nil {
			return fmt.Errorf("connector: acquire installation process lock: %w", err)
		}
		defer lock.Close()
	}
	if args[0] != "activate" {
		if _, err := os.Stat(filepath.Join(r.stateDir, "installation.json")); err == nil {
			if _, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json")); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if args[0] != "upgrade" && !(args[0] == "validate-service" && containsArgument(args[1:], "--settings-file")) {
			if err := loadSettings(filepath.Join(r.stateDir, "settings.json"), r.settingValues, r.machineState); err != nil {
				return err
			}
		}
	}
	if err := r.publishSettings(); err != nil {
		return err
	}
	if args[0] != "run" {
		defer r.closeDirectories()
	}
	switch args[0] {
	case "activate":
		return r.activate(ctx, args[1:])
	case "run":
		if len(args) != 1 {
			return errors.New("connector: run takes no arguments")
		}
		if r.installationID == "" {
			return errors.New("connector: run requires a selected activated installation")
		}
		return runAsService(ctx, "airlock-connector-"+r.config.Kind+"-"+r.installationID, r.runService)
	case "configure":
		return r.configure(ctx, args[1:])
	case "validate":
		if len(args) != 1 {
			return errors.New("connector: validate takes no arguments")
		}
		return r.validateForMode(ctx)
	case "validate-service":
		return r.validateServiceCommand(ctx, args[1:])
	case "version":
		if len(args) != 1 {
			return errors.New("connector: version takes no arguments")
		}
		_, err := fmt.Fprintf(r.config.Output, "%s %s protocol %d.%d\n", r.config.Kind, r.config.ArtifactVersion, protocol.Major, protocol.Minor)
		return err
	case "status":
		return r.status(ctx, args[1:])
	case "reconcile-job":
		return r.reconcileJob(ctx, args[1:])
	case "unregister":
		return r.unregister(ctx, args[1:])
	case "enable", "disable":
		return r.setEnabled(ctx, args[0] == "enable", args[1:])
	case "install", "uninstall", "start", "stop", "restart", "upgrade", "rollback":
		return r.serviceCommand(ctx, args[0], args[1:])
	default:
		return fmt.Errorf("connector: unknown command %q", args[0])
	}
}

func (r *Runtime) closeDirectories() {
	for _, directory := range r.directories {
		_ = directory.provider.Close()
	}
}

func installationCommandNeedsLock(command string) bool {
	switch command {
	case "activate", "configure", "unregister", "enable", "disable", "install", "uninstall", "start", "stop", "restart", "upgrade", "rollback":
		return true
	default:
		return false
	}
}

func draftStateDirectory(base string) string {
	return filepath.Join(base, "draft")
}

func (r *Runtime) installationLockPath() string {
	return filepath.Join(r.stateDir, ".installation.lock")
}

func (r *Runtime) extractInstallation(args []string) ([]string, error) {
	result := []string{args[0]}
	selector := ""
	newDraft := false
	for i := 1; i < len(args); i++ {
		if args[0] == "configure" && args[i] == "--new" {
			newDraft = true
			continue
		}
		if args[i] != "--installation" {
			result = append(result, args[i])
			continue
		}
		if selector != "" || i+1 >= len(args) {
			return nil, errors.New("connector: --installation requires one lowercase UUID")
		}
		selector = args[i+1]
		i++
	}
	if newDraft {
		if selector != "" {
			return nil, errors.New("connector: configure --new cannot be combined with --installation")
		}
		r.stateDir, r.installationID, r.ambiguousInstallations = draftStateDirectory(r.stateBase), "", false
		if err := prepareStateDirectory(r.stateDir, r.config.ServiceMode, r.config.Operations); err != nil {
			return nil, err
		}
		if r.settingValues != nil {
			if err := json.Unmarshal(r.initialSettings, r.settingValues); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	if selector == "" {
		return result, nil
	}
	if !validInstallationID(selector) {
		return nil, errors.New("connector: --installation requires a lowercase UUID")
	}
	if r.installationID != "" && r.installationID != selector {
		return nil, errors.New("connector: --installation conflicts with Config.InstallationID or AIRLOCK_CONNECTOR_INSTALLATION_ID")
	}
	if r.installationID == "" {
		ids, err := installationDirectories(r.stateBase)
		if err != nil {
			return nil, err
		}
		found := false
		for _, id := range ids {
			found = found || id == selector
		}
		if !found {
			return nil, fmt.Errorf("connector: installation %s does not exist", selector)
		}
		r.installationID, r.stateDir, r.ambiguousInstallations = selector, filepath.Join(r.stateBase, "installations", selector), false
		if r.settingValues != nil {
			if err := json.Unmarshal(r.initialSettings, r.settingValues); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func containsArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func installationArgument(args []string) string {
	for i, arg := range args {
		if arg == "--installation" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (r *Runtime) loadInstallation(path string) (installationState, error) {
	state, err := loadInstallation(path, r.machineState)
	if err != nil {
		return installationState{}, err
	}
	if state.ServiceMode == "" {
		state.ServiceMode = r.config.ServiceMode
	} else if state.ServiceMode != r.config.ServiceMode {
		return installationState{}, fmt.Errorf("connector: persisted service mode %q does not match Config.ServiceMode %q", state.ServiceMode, r.config.ServiceMode)
	}
	return state, nil
}

func (r *Runtime) configure(ctx context.Context, args []string) error {
	manager := r.serviceManager()
	rootBindings := r.directoryRootBindings()
	if manager.Installed() {
		installedSchema, err := loadSettingsSchema(filepath.Join(r.stateDir, "settings-schema.json"))
		if err != nil {
			return fmt.Errorf("connector: load installed settings schema: %w", err)
		}
		if !settingsSchemasEqual(installedSchema, r.settings, r.settingFields) {
			return errors.New("connector: candidate settings schema differs from the installed binary; provide candidate settings to upgrade")
		}
	}
	before, err := json.Marshal(r.settingValues)
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(r.stateDir, "settings.json")
	settingsSchemaPath := filepath.Join(r.stateDir, "settings-schema.json")
	statePath := filepath.Join(r.stateDir, "installation.json")
	settingsBefore, err := snapshotFile(settingsPath)
	if err != nil {
		return err
	}
	stateBefore, err := snapshotFile(statePath)
	if err != nil {
		return err
	}
	schemaBefore, err := snapshotFile(settingsSchemaPath)
	if err != nil {
		return err
	}
	state, err := r.loadInstallation(statePath)
	if err != nil {
		return err
	}
	interactive := isTerminal(r.config.Input)
	validator := r.validate
	if r.config.ServiceMode == ServiceSystem {
		validator = r.validateFields
	}
	validateProposed := func(ctx context.Context) error {
		if r.settingHandle != nil {
			r.settingHandle.publish()
		}
		if err := rootBindings.apply(r.settingValues); err != nil {
			return err
		}
		return validator(ctx)
	}
	if err := configureSettings(ctx, r.settingValues, r.settingFields, args, r.config.Input, r.config.Output, interactive, validateProposed); err != nil {
		_ = json.Unmarshal(before, r.settingValues)
		if r.settingHandle != nil {
			r.settingHandle.publish()
		}
		return errors.Join(err, rootBindings.restore())
	}
	restore := func(validationErr error) error {
		_ = json.Unmarshal(before, r.settingValues)
		if r.settingHandle != nil {
			r.settingHandle.publish()
		}
		restoreErr := errors.Join(rootBindings.restore(), settingsBefore.restore(), stateBefore.restore(), schemaBefore.restore())
		if r.config.ServiceMode == ServiceSystem && r.installationID != "" {
			restoreErr = errors.Join(restoreErr, r.serviceManager().PrepareIdentity(ctx))
		}
		return errors.Join(validationErr, restoreErr)
	}
	if err := saveSettings(settingsPath, r.settingValues, r.machineState); err != nil {
		return restore(err)
	}
	if err := saveSettingsSchema(settingsSchemaPath, r.settings, r.settingFields); err != nil {
		return restore(err)
	}
	if err := saveInstallation(statePath, state, r.machineState); err != nil {
		return restore(err)
	}
	if r.config.ServiceMode == ServiceSystem {
		if err := manager.ValidateIdentity(ctx, r.installationID, ""); err != nil {
			return restore(err)
		}
	}
	if manager.Installed() {
		manager = r.serviceManager()
		restoreDefinition, err := manager.Reconfigure(ctx)
		if err != nil {
			return restore(err)
		}
		started := time.Now().UTC()
		restartErr := manager.Stop(ctx)
		if restartErr == nil {
			restartErr = manager.Start(ctx)
		}
		if restartErr == nil {
			restartErr = r.waitReady(ctx, started)
		}
		if restartErr != nil {
			restoreErr := errors.Join(restore(nil), restoreDefinition())
			if restoreErr == nil {
				restoreErr = manager.Stop(ctx)
			}
			if restoreErr == nil {
				restoreErr = manager.Start(ctx)
			}
			if restoreErr == nil {
				restoreErr = r.waitReady(ctx, time.Now().Add(-time.Second))
			}
			return errors.Join(fmt.Errorf("connector: configured service failed validation: %w", restartErr), restoreErr)
		}
	}
	_, err = fmt.Fprintln(r.config.Output, "Configuration validated and saved.")
	return err
}

type fileSnapshot struct {
	path    string
	body    []byte
	existed bool
}

func snapshotFile(path string) (fileSnapshot, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{path: path, body: body, existed: true}, nil
}

func (s fileSnapshot) restore() error {
	if !s.existed {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return atomicWrite(s.path, s.body, 0o600)
}

func (r *Runtime) validateFields(context.Context) error {
	for _, field := range r.settingFields {
		if field.required && reflectSettingZero(r.settingValues, field.index) {
			return fmt.Errorf("connector: required setting %s is missing", field.name)
		}
	}
	return nil
}

func (r *Runtime) validateForMode(ctx context.Context) error {
	if r.config.ServiceMode == ServiceUser {
		return r.validate(ctx)
	}
	return r.serviceManager().ValidateIdentity(ctx, r.installationID, "")
}

func (r *Runtime) validate(ctx context.Context) error {
	if err := r.validateFields(ctx); err != nil {
		return err
	}
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

func (r *Runtime) status(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("connector: status takes no arguments")
	}
	state, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json"))
	if err != nil {
		return err
	}
	service, serviceErr := r.serviceManager().Status(ctx)
	configured := false
	_, settingsErr := os.Stat(filepath.Join(r.stateDir, "settings.json"))
	if schema, schemaErr := loadSettingsSchema(filepath.Join(r.stateDir, "settings-schema.json")); schemaErr == nil && settingsErr == nil && settingsSchemasEqual(schema, r.settings, r.settingFields) {
		configured = r.validateForMode(ctx) == nil
	}
	_, err = fmt.Fprintf(r.config.Output, "configured=%t activated=%t enabled=%t service=%s\n", configured, state.Credential != "", state.Enabled, service)
	if err != nil {
		return err
	}
	return serviceErr
}

func (r *Runtime) reconcileJob(ctx context.Context, args []string) error {
	if len(args) != 3 || (args[1] != "--output-json" && args[1] != "--error") {
		return errors.New("connector: reconcile-job requires <idempotency-id> (--output-json <json> | --error <message>)")
	}
	recordPath := r.idempotencyPath(args[0])
	lock, err := acquireFileLock(ctx, recordPath+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	record, found, err := r.loadIdempotency(recordPath)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("connector: idempotency record does not exist")
	}
	if record.Status != "indeterminate" {
		return fmt.Errorf("connector: idempotency record is already %s", record.Status)
	}
	if args[1] == "--output-json" {
		output := json.RawMessage(args[2])
		if len(output) == 0 || len(output) > protocol.MaxJobPayloadBytes || !json.Valid(output) {
			return errors.New("connector: reconciliation output must be valid bounded JSON")
		}
		record.Status, record.Output, record.Error, record.ReservedBytes = "completed", output, "", 0
	} else {
		if strings.TrimSpace(args[2]) == "" {
			return errors.New("connector: reconciliation error is required")
		}
		record.Status, record.Output, record.Error, record.ReservedBytes = "failed", nil, boundedError(errors.New(args[2])), 0
	}
	if err := r.saveIdempotency(recordPath, record); err != nil {
		return err
	}
	_, err = fmt.Fprintln(r.config.Output, "Idempotency record reconciled.")
	return err
}

func (r *Runtime) setEnabled(ctx context.Context, enabled bool, args []string) error {
	if len(args) != 0 {
		return errors.New("connector: enable and disable take no arguments")
	}
	path := filepath.Join(r.stateDir, "installation.json")
	state, err := r.loadInstallation(path)
	if err != nil {
		return err
	}
	previous := state.Enabled
	state.Enabled = enabled
	if err := saveInstallation(path, state, r.machineState); err != nil {
		return err
	}
	manager := r.serviceManager()
	if enabled {
		err = manager.Enable(ctx)
	} else {
		err = manager.Disable(ctx)
	}
	if err != nil {
		state.Enabled = previous
		return errors.Join(err, saveInstallation(path, state, r.machineState))
	}
	return nil
}

func (r *Runtime) serviceCommand(ctx context.Context, command string, args []string) error {
	if command != "upgrade" && len(args) != 0 {
		return fmt.Errorf("connector: %s takes no arguments", command)
	}
	manager := r.serviceManager()
	switch command {
	case "install":
		state, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json"))
		if err != nil {
			return err
		}
		if r.installationID == "" || state.InstallationID != r.installationID || state.Credential == "" {
			return errors.New("connector: select and activate an installation before install")
		}
		if err := serviceLifecycleGuard(command, state, manager.Installed()); err != nil {
			return err
		}
		if err := r.validateForMode(ctx); err != nil {
			return err
		}
		return manager.Install(ctx)
	case "uninstall":
		return manager.Uninstall(ctx)
	case "start":
		started := time.Now().UTC()
		if err := manager.Start(ctx); err != nil {
			return err
		}
		return r.waitReady(ctx, started)
	case "stop":
		return manager.Stop(ctx)
	case "restart":
		if err := manager.Stop(ctx); err != nil {
			return err
		}
		started := time.Now().UTC()
		if err := manager.Start(ctx); err != nil {
			return err
		}
		return r.waitReady(ctx, started)
	case "upgrade":
		state, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json"))
		if err != nil {
			return err
		}
		if err := serviceLifecycleGuard(command, state, manager.Installed()); err != nil {
			return err
		}
		return r.upgradeService(ctx, nil, args)
	case "rollback":
		state, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json"))
		if err != nil {
			return err
		}
		if err := serviceLifecycleGuard(command, state, manager.Installed()); err != nil {
			return err
		}
		return r.rollbackService(ctx, manager)
	}
	panic("unreachable")
}

func serviceLifecycleGuard(command string, state installationState, installed bool) error {
	switch command {
	case "install":
		if installed {
			return errors.New("connector: service is already installed; use upgrade")
		}
	case "upgrade", "rollback":
		if !state.Enabled {
			return fmt.Errorf("connector: installation is disabled; enable it before %s", command)
		}
	}
	return nil
}

func (r *Runtime) upgradeService(ctx context.Context, manager serviceManager, args []string) error {
	activateSettings, cleanup, err := r.prepareUpgradeSettings(ctx, args)
	if err != nil {
		return err
	}
	defer cleanup()
	if manager == nil {
		manager = r.serviceManager()
	}
	previousRollback, err := snapshotFile(filepath.Join(r.stateDir, "rollback-state.json"))
	if err != nil {
		return err
	}
	if err := r.retainRollbackState(); err != nil {
		return err
	}
	started := time.Now().UTC()
	rollbackReady, err := manager.Upgrade(ctx, activateSettings)
	if err != nil {
		if !rollbackReady {
			return errors.Join(err, previousRollback.restore())
		}
		return errors.Join(fmt.Errorf("connector: service upgrade failed: %w", err), r.rollbackService(ctx, manager))
	}
	if !rollbackReady {
		return errors.Join(errors.New("connector: service upgrade did not retain a rollback binary"), previousRollback.restore())
	}
	if err := r.waitReady(ctx, started); err != nil {
		rollbackErr := r.rollbackService(ctx, manager)
		return errors.Join(fmt.Errorf("connector: upgraded service did not become ready: %w", err), rollbackErr)
	}
	return nil
}

func (r *Runtime) prepareUpgradeSettings(ctx context.Context, args []string) (func() error, func(), error) {
	rootBindings := r.directoryRootBindings()
	schemaPath := filepath.Join(r.stateDir, "settings-schema.json")
	installedSchema, err := loadSettingsSchema(schemaPath)
	if err != nil {
		return nil, nil, fmt.Errorf("connector: load installed settings schema: %w", err)
	}
	if r.settingValues != nil {
		if err := json.Unmarshal(r.initialSettings, r.settingValues); err != nil {
			return nil, nil, err
		}
		encoded, err := readSettings(filepath.Join(r.stateDir, "settings.json"), r.machineState)
		if errors.Is(err, os.ErrNotExist) {
			encoded = []byte("{}")
		} else if err != nil {
			return nil, nil, err
		}
		if err := migrateSettings(r.settingValues, r.settingFields, installedSchema, encoded); err != nil {
			return nil, nil, err
		}
		validator := r.validate
		if r.config.ServiceMode == ServiceSystem {
			validator = r.validateFields
		}
		validateProposed := func(ctx context.Context) error {
			if r.settingHandle != nil {
				r.settingHandle.publish()
			}
			if err := rootBindings.apply(r.settingValues); err != nil {
				return err
			}
			return validator(ctx)
		}
		if err := configureSettingsCommand(ctx, "upgrade", r.settingValues, r.settingFields, args, r.config.Input, r.config.Output, isTerminal(r.config.Input), validateProposed); err != nil {
			_ = rootBindings.restore()
			if r.settingHandle != nil {
				r.settingHandle.publish()
			}
			return nil, nil, err
		}
	} else {
		if len(args) != 0 {
			return nil, nil, errors.New("connector: upgrade has no settings flags")
		}
		if err := r.validate(ctx); err != nil {
			return nil, nil, err
		}
	}
	stagedSettings := filepath.Join(r.stateDir, ".upgrade-settings.json")
	stagedSchema := filepath.Join(r.stateDir, ".upgrade-settings-schema.json")
	cleanup := func() {
		_ = os.Remove(stagedSettings)
		_ = os.Remove(stagedSchema)
	}
	cleanup()
	if err := saveSettings(stagedSettings, r.settingValues, r.machineState); err != nil {
		return nil, nil, errors.Join(err, rootBindings.restore())
	}
	if err := saveSettingsSchema(stagedSchema, r.settings, r.settingFields); err != nil {
		cleanup()
		return nil, nil, errors.Join(err, rootBindings.restore())
	}
	manager := r.serviceManager()
	if r.config.ServiceMode == ServiceSystem {
		if err := manager.ValidateIdentity(ctx, r.installationID, stagedSettings); err != nil {
			cleanup()
			return nil, nil, errors.Join(err, rootBindings.restore())
		}
	}
	activate := func() error {
		settings, err := os.ReadFile(stagedSettings)
		if err != nil {
			return err
		}
		schema, err := os.ReadFile(stagedSchema)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(r.stateDir, "settings.json"), settings, 0o600); err != nil {
			return err
		}
		if err := atomicWrite(schemaPath, schema, 0o600); err != nil {
			return err
		}
		return manager.PrepareIdentity(ctx)
	}
	return activate, cleanup, nil
}

func (r *Runtime) serviceManager() serviceManager {
	roots := make([]string, 0, len(r.directories))
	seen := make(map[string]bool, len(r.directories))
	for _, directory := range r.directories {
		if !directory.descriptor.Write || directory.provider.path == "" {
			continue
		}
		root := filepath.Clean(directory.provider.path)
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return newServiceManager(r.config.Kind, r.stateDir, r.config.ServiceMode, r.config.Operations, roots)
}

type directoryRootBinding struct {
	provider *LocalDirectoryProvider
	field    int
	previous string
}

type directoryRootBindings []directoryRootBinding

func (r *Runtime) directoryRootBindings() directoryRootBindings {
	bindings := make(directoryRootBindings, 0, len(r.directories))
	for _, directory := range r.directories {
		if directory.provider.binding != nil {
			bindings = append(bindings, directoryRootBinding{
				provider: directory.provider,
				field:    directory.provider.binding.field.index,
				previous: directory.provider.path,
			})
		}
	}
	return bindings
}

func (b directoryRootBindings) apply(settings any) error {
	if settings == nil {
		return nil
	}
	value := reflect.ValueOf(settings).Elem()
	applied := make(directoryRootBindings, 0, len(b))
	for _, binding := range b {
		path := value.Field(binding.field).String()
		if err := binding.provider.rebind(path); err != nil {
			_ = applied.restore()
			return fmt.Errorf("connector: bind local directory setting: %w", err)
		}
		applied = append(applied, binding)
	}
	return nil
}

func (b directoryRootBindings) restore() error {
	var result error
	for _, binding := range b {
		result = errors.Join(result, binding.provider.rebind(binding.previous))
	}
	return result
}

func (r *Runtime) retainRollbackState() error {
	installation, err := os.ReadFile(filepath.Join(r.stateDir, "installation.json"))
	if err != nil {
		return fmt.Errorf("connector: retain rollback installation state: %w", err)
	}
	settings, settingsErr := os.ReadFile(filepath.Join(r.stateDir, "settings.json"))
	settingsPresent := settingsErr == nil
	if settingsErr != nil && !errors.Is(settingsErr, os.ErrNotExist) {
		return fmt.Errorf("connector: retain rollback settings: %w", settingsErr)
	}
	settingsSchema, err := os.ReadFile(filepath.Join(r.stateDir, "settings-schema.json"))
	if err != nil {
		return fmt.Errorf("connector: retain rollback settings schema: %w", err)
	}
	return saveRollbackState(filepath.Join(r.stateDir, "rollback-state.json"), rollbackState{
		ServiceMode: r.config.ServiceMode, InstallationID: r.installationID, Installation: installation,
		Settings: settings, SettingsPresent: settingsPresent, SettingsSchema: settingsSchema,
	})
}

func (r *Runtime) restoreRollbackState() error {
	state, err := loadRollbackState(filepath.Join(r.stateDir, "rollback-state.json"))
	if err != nil {
		return fmt.Errorf("connector: load retained rollback state: %w", err)
	}
	if state.ServiceMode != r.config.ServiceMode || state.InstallationID != r.installationID {
		return errors.New("connector: retained rollback state does not match the selected installation and service mode")
	}
	if err := atomicWrite(filepath.Join(r.stateDir, "installation.json"), state.Installation, 0o600); err != nil {
		return err
	}
	settingsPath := filepath.Join(r.stateDir, "settings.json")
	if state.SettingsPresent {
		if err := atomicWrite(settingsPath, state.Settings, 0o600); err != nil {
			return err
		}
	} else if err := os.Remove(settingsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWrite(filepath.Join(r.stateDir, "settings-schema.json"), state.SettingsSchema, 0o600); err != nil {
		return err
	}
	return nil
}

func (r *Runtime) rollbackService(ctx context.Context, manager serviceManager) error {
	digest, err := manager.RollbackDigest()
	if err != nil {
		return fmt.Errorf("connector: read retained rollback binary: %w", err)
	}
	if err := manager.Stop(ctx); err != nil {
		return err
	}
	if err := r.restoreRollbackState(); err != nil {
		return err
	}
	if err := manager.PrepareIdentity(ctx); err != nil {
		return err
	}
	started := time.Now().UTC()
	if err := manager.Rollback(ctx); err != nil {
		return err
	}
	return r.waitReadyArtifact(ctx, started, "", digest)
}

func (r *Runtime) waitReady(ctx context.Context, after time.Time) error {
	return r.waitReadyArtifact(ctx, after, r.config.ArtifactVersion, r.artifactDigest)
}

func (r *Runtime) waitReadyArtifact(ctx context.Context, after time.Time, artifactVersion, artifactDigest string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	path := filepath.Join(r.stateDir, "runtime.json")
	for {
		status, err := loadRuntimeStatus(path)
		artifactMatches := artifactVersion == "" || status.ArtifactVersion == artifactVersion
		digestMatches := artifactDigest == "" || status.ArtifactDigest == artifactDigest
		if err == nil && status.Readiness == protocol.ReadinessReady && artifactMatches && digestMatches && status.UpdatedAt.After(after) {
			return nil
		}
		if err == nil && status.UpdatedAt.After(after) && artifactMatches && digestMatches && status.Readiness == protocol.ReadinessUnhealthy {
			return fmt.Errorf("connector: service is unhealthy: %s", status.Message)
		}
		if err == nil && status.UpdatedAt.After(after) && artifactMatches && digestMatches && status.Readiness == protocol.ReadinessOffline {
			return errors.New("connector: service exited before becoming ready")
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("connector: service readiness timeout: %w", waitCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *Runtime) runService(ctx context.Context) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	var initializedDirectories []*LocalDirectoryProvider
	defer func() {
		cancelRun()
		for i := len(initializedDirectories) - 1; i >= 0; i-- {
			initializedDirectories[i].Close()
		}
	}()
	statusPath := filepath.Join(r.stateDir, "runtime.json")
	_ = saveRuntimeStatus(statusPath, runtimeStatus{Readiness: protocol.ReadinessStarting, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: r.artifactDigest})
	defer func() {
		_ = saveRuntimeStatus(statusPath, runtimeStatus{Readiness: protocol.ReadinessOffline, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: r.artifactDigest})
	}()
	if err := r.validate(ctx); err != nil {
		_ = saveRuntimeStatus(statusPath, runtimeStatus{Readiness: protocol.ReadinessUnhealthy, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: r.artifactDigest, Message: err.Error()})
		return err
	}
	state, err := r.loadInstallation(filepath.Join(r.stateDir, "installation.json"))
	if err != nil {
		return err
	}
	if !state.Enabled {
		return errors.New("connector: installation is disabled")
	}
	if state.Credential == "" || state.InstallationID == "" || state.AirlockURL == "" {
		return errors.New("connector: activation is required")
	}
	for _, directory := range r.directories {
		directory.provider.setOrigins(state.StorageOrigins)
		if !directory.provider.configured() {
			continue
		}
		if err := directory.provider.initialize(); err != nil {
			return fmt.Errorf("connector: directory %s: %w", directory.descriptor.Name, err)
		}
		initializedDirectories = append(initializedDirectories, directory.provider)
	}
	if err := r.runStartHooks(runCtx); err != nil {
		return fmt.Errorf("connector: start: %w", err)
	}
	client, err := newProtocolClient(state.AirlockURL, state.Credential, r.config.HTTPClient, r.config.AllowInsecureHTTP)
	if err != nil {
		return err
	}
	if err := client.post(ctx, "/api/connectors/v1/interface", r.Manifest(), nil); err != nil {
		return fmt.Errorf("connector: publish interface: %w", err)
	}
	r.healthy.Store(true)
	if err := saveRuntimeStatus(statusPath, runtimeStatus{Readiness: protocol.ReadinessReady, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: r.artifactDigest}); err != nil {
		return err
	}
	backgroundCtx, cancelBackground := context.WithCancel(runCtx)
	var background sync.WaitGroup
	background.Add(2)
	defer func() {
		cancelBackground()
		background.Wait()
	}()
	go func() {
		defer background.Done()
		r.heartbeat(backgroundCtx, client, statusPath)
	}()
	go func() {
		defer background.Done()
		r.pollCancellations(backgroundCtx, client)
	}()
	semaphore := make(chan struct{}, r.config.MaxConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		if !r.healthy.Load() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		var job protocol.JobRequest
		found, err := client.poll(ctx, "/api/connectors/v1/jobs/request", protocol.Handshake{ProtocolMajor: protocol.Major, ProtocolMinor: protocol.Minor, Features: r.Manifest().Features}, &job)
		if err != nil {
			<-semaphore
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}
		if !found {
			<-semaphore
			continue
		}
		if !r.healthy.Load() {
			completion := protocol.JobCompletion{AttemptToken: job.AttemptToken, Status: "error", Error: "connector became unhealthy before dispatch"}
			if err := client.post(context.WithoutCancel(ctx), "/api/connectors/v1/jobs/"+url.PathEscape(job.JobID)+"/complete", completion, nil); err != nil {
				_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
			}
			<-semaphore
			continue
		}
		workers.Add(1)
		go func() {
			defer func() { <-semaphore; workers.Done() }()
			completion := r.dispatch(ctx, client, job)
			path := "/api/connectors/v1/jobs/" + url.PathEscape(job.JobID) + "/complete"
			if err := client.post(context.WithoutCancel(ctx), path, completion, nil); err != nil {
				_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
			}
		}()
	}
}

func (r *Runtime) heartbeat(ctx context.Context, client *protocolClient, statusPath string) {
	ticker := time.NewTicker(r.config.HeartbeatInterval)
	defer ticker.Stop()
	send := func() {
		manifest := r.Manifest()
		readiness, message := protocol.ReadinessReady, ""
		if err := r.validate(ctx); err != nil {
			readiness, message = protocol.ReadinessUnhealthy, err.Error()
		}
		r.healthy.Store(readiness == protocol.ReadinessReady)
		if err := saveRuntimeStatus(statusPath, runtimeStatus{Readiness: readiness, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: r.artifactDigest, Message: message}); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
		}
		request := protocol.HeartbeatRequest{Handshake: protocol.Handshake{ProtocolMajor: protocol.Major, ProtocolMinor: protocol.Minor, Features: manifest.Features}, Readiness: readiness, ArtifactVersion: r.config.ArtifactVersion, ArtifactDigest: manifest.ArtifactDigest, InterfaceHash: manifest.InterfaceHash, ActiveAttempts: r.activeAttempts(), Error: message}
		if err := client.post(ctx, "/api/connectors/v1/heartbeat", request, nil); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintln(r.config.ErrorOutput, err)
		}
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

func (r *Runtime) activeAttempts() []protocol.ActiveAttempt {
	r.activeMu.Lock()
	attempts := make([]protocol.ActiveAttempt, 0, len(r.active))
	for key := range r.active {
		attempts = append(attempts, protocol.ActiveAttempt{JobID: key.jobID, AttemptToken: key.attemptToken})
	}
	r.activeMu.Unlock()
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].JobID < attempts[j].JobID })
	return attempts
}

func (r *Runtime) pollCancellations(ctx context.Context, client *protocolClient) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var cancellations []protocol.ActiveAttempt
		found, err := client.poll(ctx, "/api/connectors/v1/jobs/cancellations", protocol.Handshake{ProtocolMajor: protocol.Major, ProtocolMinor: protocol.Minor, Features: r.Manifest().Features}, &cancellations)
		if err == nil && found {
			for _, cancellation := range cancellations {
				r.cancelJob(cancellation.JobID, cancellation.AttemptToken)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runtime) dispatch(parent context.Context, client *protocolClient, job protocol.JobRequest) protocol.JobCompletion {
	completion := protocol.JobCompletion{AttemptToken: job.AttemptToken, Status: "error"}
	if job.JobID == "" || job.AttemptToken == "" || job.Deadline.IsZero() || !job.Deadline.After(time.Now()) {
		completion.Error = "invalid or expired job delivery"
		return completion
	}
	if len(job.Input) == 0 || len(job.Input) > protocol.MaxJobPayloadBytes || !json.Valid(job.Input) {
		completion.Error = "invalid or oversized job input"
		return completion
	}
	ctx, cancel := context.WithDeadline(parent, job.Deadline)
	key := activeAttemptKey{jobID: job.JobID, attemptToken: job.AttemptToken}
	r.activeMu.Lock()
	r.active[key] = activeJob{cancel: cancel}
	r.activeMu.Unlock()
	defer func() { cancel(); r.activeMu.Lock(); delete(r.active, key); r.activeMu.Unlock() }()
	execution := &executionContext{Context: ctx, job: job, client: client}
	var output json.RawMessage
	var err error
	switch job.Kind {
	case protocol.JobKindCommand:
		output, err = r.dispatchCommand(execution, job)
	default:
		output, err = r.dispatchDirectory(execution, job)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			completion.Status = "canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			completion.Status = "timeout"
		}
		completion.Error = err.Error()
		return completion
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			completion.Status = "timeout"
		} else {
			completion.Status = "canceled"
		}
		completion.Error = err.Error()
		return completion
	}
	completion.Status, completion.Output = "success", output
	return completion
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
	ctx.Context = commandCtx
	ctx.mode = command.descriptor.Mode
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

type executionContext struct {
	context.Context
	job      protocol.JobRequest
	client   *protocolClient
	sequence int64
	mode     protocol.CommandMode
}

func (c *executionContext) JobID() string         { return c.job.JobID }
func (c *executionContext) IdempotencyID() string { return c.job.IdempotencyID }
func (c *executionContext) Progress(phase, message string, completed, total int64) error {
	if c.mode != protocol.CommandModeJob {
		return errors.New("connector: progress is available only for job-mode commands")
	}
	if completed < 0 || total < 0 || (total > 0 && completed > total) {
		return errors.New("connector: invalid progress")
	}
	c.sequence++
	event := protocol.JobEvent{AttemptToken: c.job.AttemptToken, Sequence: c.sequence, Phase: phase, Message: message, Completed: completed, Total: total, Time: time.Now().UTC()}
	return c.client.post(c, "/api/connectors/v1/jobs/"+url.PathEscape(c.job.JobID)+"/events", event, nil)
}

type idempotencyRecord struct {
	Version       int `json:"version"`
	Status, JobID string
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
	body, err = unprotectBytes(body, r.machineState)
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
	body, err = protectBytes(body, r.machineState)
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
		size := max(info.Size(), record.ReservedBytes)
		updatedAt := record.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = record.CreatedAt
		}
		if record.Status != "indeterminate" && !updatedAt.IsZero() && now.Sub(updatedAt) >= idempotencyRetention {
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
	count, bytes, err := r.compactIdempotency(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if count >= maxIdempotencyRecords || bytes+idempotencyRecordReserve > maxIdempotencyBytes {
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
	if len(message) <= maxIdempotencyErrorBytes {
		return message
	}
	return message[:maxIdempotencyErrorBytes]
}

type protocolClient struct {
	baseURL, credential string
	http                *http.Client
}

func newProtocolClient(baseURL, credential string, client *http.Client, insecure bool) (*protocolClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(insecure && parsed.Scheme == "http")) || parsed.Path != "" {
		return nil, errors.New("connector: Airlock URL must be an HTTPS origin")
	}
	return &protocolClient{baseURL: strings.TrimSuffix(baseURL, "/"), credential: credential, http: noRedirectClient(client)}, nil
}

func noRedirectClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
func (c *protocolClient) post(ctx context.Context, path string, request, result any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(encoded) > protocol.MaxEnvelopeBytes {
		return fmt.Errorf("connector: protocol envelope exceeds %d bytes", protocol.MaxEnvelopeBytes)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.credential)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("connector: POST %s returned %d: %s", path, response.StatusCode, body)
	}
	if result != nil {
		return decodeBounded(response.Body, result, maximumProtocolBody)
	}
	return nil
}
func (c *protocolClient) poll(ctx context.Context, path string, request any, result any) (bool, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	if len(encoded) > protocol.MaxEnvelopeBytes {
		return false, fmt.Errorf("connector: protocol envelope exceeds %d bytes", protocol.MaxEnvelopeBytes)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return false, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.credential)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("connector: poll returned HTTP %d", response.StatusCode)
	}
	return true, decodeBounded(response.Body, result, maximumProtocolBody)
}

func decodeBounded(reader io.Reader, result any, maximum int64) error {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > maximum {
		return errors.New("connector: JSON body exceeds limit")
	}
	return strictUnmarshal(body, result)
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
func isTerminal(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
func reflectSettingZero(settings any, index int) bool {
	return reflect.ValueOf(settings).Elem().Field(index).IsZero()
}
