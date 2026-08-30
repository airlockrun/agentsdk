// Package protocol defines the JSON protocol exchanged by connector binaries
// and Airlock. It has no platform or Agents SDK runtime dependencies.
package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Major = 1
	Minor = 0

	// MaxEnvelopeBytes matches Airlock's connector protocol request limit.
	MaxEnvelopeBytes = 1 << 20
	// MaxJobPayloadBytes reserves enough room for the JSON completion envelope,
	// attempt token, status, and escaping under MaxEnvelopeBytes.
	MaxJobPayloadBytes      = MaxEnvelopeBytes - (64 << 10)
	MaxInlineFileBytes      = (MaxJobPayloadBytes * 3 / 4) - (16 << 10)
	MaxManifestBytes        = 4 << 20
	MaxSchemaBytes          = 512 << 10
	MaxTransferPartBytes    = 16 << 20
	MaxTransferParts        = 10000
	MaxDirectoryScanEntries = 10000
	MaxActiveAttempts       = 256
)

var (
	contractIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*\.[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*(?:\.[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*)+$`)
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	kindPattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

var reservedSettingNames = map[string]bool{
	"airlock": true, "check": true, "installation": true, "no-browser": true,
	"no-wait": true, "non-interactive": true, "wait": true, "new": true,
	"draft": true, "identity": true, "service-name": true, "result": true,
	"settings-file": true, "output-json": true, "error": true,
}

type CommandMode string

const (
	CommandModeUnary CommandMode = "unary"
	CommandModeJob   CommandMode = "job"
)

type CommandDescriptor struct {
	Name             string          `json:"name"`
	Revision         int             `json:"revision"`
	Description      string          `json:"description,omitempty"`
	Mode             CommandMode     `json:"mode"`
	InputSchema      json.RawMessage `json:"inputSchema"`
	OutputSchema     json.RawMessage `json:"outputSchema"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
}

type DirectoryDescriptor struct {
	Name        string `json:"name"`
	Revision    int    `json:"revision"`
	Description string `json:"description,omitempty"`
	Read        bool   `json:"read"`
	Write       bool   `json:"write"`
	List        bool   `json:"list"`
}

type SettingDescriptor struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type Interface struct {
	Kind            string                `json:"kind"`
	ContractID      string                `json:"contractId"`
	Name            string                `json:"name"`
	Description     string                `json:"description"`
	ArtifactVersion string                `json:"artifactVersion"`
	Commands        []CommandDescriptor   `json:"commands"`
	Directories     []DirectoryDescriptor `json:"directories"`
}

type Manifest struct {
	ProtocolMajor  int                 `json:"protocolMajor"`
	ProtocolMinor  int                 `json:"protocolMinor"`
	Features       []string            `json:"features"`
	Targets        []string            `json:"targets"`
	ServiceMode    string              `json:"serviceMode"`
	Interface      Interface           `json:"interface"`
	InterfaceHash  string              `json:"interfaceHash"`
	ArtifactDigest string              `json:"artifactDigest"`
	Settings       []SettingDescriptor `json:"settings"`
}

type Requirement struct {
	ContractID  string                `json:"contractId"`
	Commands    []CommandDescriptor   `json:"commands"`
	Directories []DirectoryDescriptor `json:"directories"`
}

type Handshake struct {
	ProtocolMajor int      `json:"protocolMajor"`
	ProtocolMinor int      `json:"protocolMinor"`
	Features      []string `json:"features"`
}

type DeviceCodeRequest struct {
	Manifest Manifest `json:"manifest"`
}

type DeviceCodeResponse struct {
	DeviceSecret        string    `json:"deviceSecret"`
	UserCode            string    `json:"userCode"`
	VerificationURL     string    `json:"verificationUrl"`
	ExpiresAt           time.Time `json:"expiresAt"`
	PollIntervalSeconds int       `json:"pollIntervalSeconds"`
	StorageOrigins      []string  `json:"storageOrigins,omitempty"`
}

type EnrollmentRequest struct {
	DeviceSecret string `json:"deviceSecret"`
}

type EnrollmentResponse struct {
	Status         string   `json:"status"`
	InstallationID string   `json:"installationId,omitempty"`
	Credential     string   `json:"credential,omitempty"`
	StorageOrigins []string `json:"storageOrigins,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type Readiness string

const (
	ReadinessOffline            Readiness = "offline"
	ReadinessStarting           Readiness = "starting"
	ReadinessNeedsConfiguration Readiness = "needs_configuration"
	ReadinessIncompatible       Readiness = "incompatible_protocol"
	ReadinessUnhealthy          Readiness = "unhealthy"
	ReadinessReady              Readiness = "ready"
)

type HeartbeatRequest struct {
	Handshake       Handshake       `json:"handshake"`
	Readiness       Readiness       `json:"readiness"`
	ArtifactVersion string          `json:"artifactVersion"`
	ArtifactDigest  string          `json:"artifactDigest"`
	InterfaceHash   string          `json:"interfaceHash"`
	ActiveAttempts  []ActiveAttempt `json:"activeAttempts,omitempty"`
	Error           string          `json:"error,omitempty"`
}

type ActiveAttempt struct {
	JobID        string `json:"jobId"`
	AttemptToken string `json:"attemptToken"`
}

type JobKind string

const (
	JobKindCommand         JobKind = "command"
	JobKindDirectoryList   JobKind = "directory_list"
	JobKindDirectoryStat   JobKind = "directory_stat"
	JobKindDirectoryRead   JobKind = "directory_read"
	JobKindDirectoryWrite  JobKind = "directory_write"
	JobKindDirectoryDelete JobKind = "directory_delete"
	JobKindDirectoryMove   JobKind = "directory_move"
	JobKindDirectoryImport JobKind = "directory_import"
	JobKindDirectoryExport JobKind = "directory_export"
	JobKindCancel          JobKind = "cancel"
)

type JobRequest struct {
	JobID            string          `json:"jobId"`
	AttemptToken     string          `json:"attemptToken"`
	IdempotencyID    string          `json:"idempotencyId"`
	Kind             JobKind         `json:"kind"`
	Operation        string          `json:"operation"`
	Revision         int             `json:"revision"`
	Mode             CommandMode     `json:"mode,omitempty"`
	InputSchemaHash  string          `json:"inputSchemaHash,omitempty"`
	OutputSchemaHash string          `json:"outputSchemaHash,omitempty"`
	Input            json.RawMessage `json:"input"`
	Deadline         time.Time       `json:"deadline"`
}

type JobEvent struct {
	AttemptToken string    `json:"attemptToken"`
	Sequence     int64     `json:"sequence"`
	Phase        string    `json:"phase"`
	Message      string    `json:"message,omitempty"`
	Completed    int64     `json:"completed,omitempty"`
	Total        int64     `json:"total,omitempty"`
	Time         time.Time `json:"time"`
}

type JobCompletion struct {
	AttemptToken string          `json:"attemptToken"`
	Status       string          `json:"status"`
	Output       json.RawMessage `json:"output,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type CommandCallRequest struct {
	RequestID        string          `json:"requestId,omitempty"`
	Revision         int             `json:"revision"`
	Mode             CommandMode     `json:"mode"`
	InputSchemaHash  string          `json:"inputSchemaHash"`
	OutputSchemaHash string          `json:"outputSchemaHash"`
	Input            json.RawMessage `json:"input"`
	Deadline         *time.Time      `json:"deadline,omitempty"`
}

type CommandCallResponse struct {
	JobID  string          `json:"jobId"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type DirectoryListRequest struct {
	Path   string `json:"path"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type DirectoryEntry struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	Mode        uint32    `json:"mode"`
	Directory   bool      `json:"directory"`
	ContentType string    `json:"contentType,omitempty"`
	ModifiedAt  time.Time `json:"modifiedAt"`
}

type DirectoryListResponse struct {
	Entries    []DirectoryEntry `json:"entries"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type DirectoryReadRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
}

type DirectoryReadResponse struct {
	Entry DirectoryEntry `json:"entry"`
	Data  []byte         `json:"data"`
}

type DirectoryWriteRequest struct {
	Path      string `json:"path"`
	Data      []byte `json:"data"`
	Overwrite bool   `json:"overwrite"`
}

type DirectoryMoveRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

type TransferGrant struct {
	URL            string            `json:"url"`
	ExpiresAt      time.Time         `json:"expiresAt"`
	MaximumSize    int64             `json:"maximumSize"`
	ExpectedSize   int64             `json:"expectedSize,omitempty"`
	ExpectedSHA256 string            `json:"expectedSha256,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
}

type DirectoryImportRequest struct {
	Path      string        `json:"path"`
	Overwrite bool          `json:"overwrite"`
	Grant     TransferGrant `json:"grant"`
}

type UploadPartGrant struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type DirectoryExportRequest struct {
	Path     string            `json:"path"`
	PartSize int64             `json:"partSize"`
	Parts    []UploadPartGrant `json:"parts"`
	Grant    TransferGrant     `json:"grant"`
}

type UploadedPart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

type DirectoryExportResponse struct {
	Parts  []UploadedPart `json:"parts"`
	Size   int64          `json:"size"`
	SHA256 string         `json:"sha256"`
}

type TargetSelector struct {
	ResourceIDs []string          `json:"resourceIds,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type OrchestrationStrategy string

const (
	StrategyParallel OrchestrationStrategy = "parallel"
	StrategySerial   OrchestrationStrategy = "serial"
	StrategyRolling  OrchestrationStrategy = "rolling"
	StrategyCanary   OrchestrationStrategy = "canary"
	StrategyQuorum   OrchestrationStrategy = "quorum"
)

type OfflinePolicy string

const (
	OfflineFail   OfflinePolicy = "fail"
	OfflineSkip   OfflinePolicy = "skip"
	OfflineWait   OfflinePolicy = "wait"
	OfflineCancel OfflinePolicy = "cancel"
)

type OrchestrationRequest struct {
	RequestID      string                `json:"requestId"`
	Targets        TargetSelector        `json:"targets"`
	Strategy       OrchestrationStrategy `json:"strategy"`
	OfflinePolicy  OfflinePolicy         `json:"offlinePolicy"`
	MaxConcurrency int                   `json:"maxConcurrency,omitempty"`
	BatchSize      int                   `json:"batchSize,omitempty"`
	CanaryCount    int                   `json:"canaryCount,omitempty"`
	Quorum         int                   `json:"quorum,omitempty"`
	Deadline       *time.Time            `json:"deadline,omitempty"`
	Command        CommandCallRequest    `json:"command"`
}

func ValidateContractID(id string) error {
	if len(id) > 253 || !contractIDPattern.MatchString(id) {
		return fmt.Errorf("connector protocol: contract ID %q must be a reverse-domain identifier", id)
	}
	return nil
}

func ValidateName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("connector protocol: %s name %q must match %s", kind, name, namePattern)
	}
	return nil
}

func ValidateKind(kind string) error {
	if len(kind) > 63 || !kindPattern.MatchString(kind) {
		return fmt.Errorf("connector protocol: connector kind %q must contain lowercase letters, digits, and internal hyphens", kind)
	}
	return nil
}

func ValidateArtifactDigest(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || value != hex.EncodeToString(decoded) {
		return errors.New("connector protocol: artifact digest must be a lowercase SHA-256 digest")
	}
	return nil
}

func CanonicalJSON(value []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("connector protocol: multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(decoded)
}

func HashJSON(value []byte) (string, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func InterfaceDigest(value Interface) (string, error) {
	canonical := value
	canonical.Commands = append([]CommandDescriptor(nil), value.Commands...)
	canonical.Directories = append([]DirectoryDescriptor(nil), value.Directories...)
	sort.Slice(canonical.Commands, func(i, j int) bool { return canonical.Commands[i].Name < canonical.Commands[j].Name })
	sort.Slice(canonical.Directories, func(i, j int) bool { return canonical.Directories[i].Name < canonical.Directories[j].Name })
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return HashJSON(encoded)
}

func ValidateRequirement(r Requirement) error {
	if err := ValidateContractID(r.ContractID); err != nil {
		return err
	}
	return validateDescriptors(r.Commands, r.Directories, true)
}

func ValidateManifest(m Manifest) error {
	if m.ProtocolMajor != Major || m.ProtocolMinor < 0 {
		return fmt.Errorf("connector protocol: unsupported protocol %d.%d", m.ProtocolMajor, m.ProtocolMinor)
	}
	if err := ValidateKind(m.Interface.Kind); err != nil {
		return err
	}
	if err := ValidateContractID(m.Interface.ContractID); err != nil {
		return err
	}
	if err := ValidateArtifactDigest(m.ArtifactDigest); err != nil {
		return err
	}
	if m.ServiceMode != "user" && m.ServiceMode != "system" {
		return errors.New("connector protocol: service mode must be user or system")
	}
	if strings.TrimSpace(m.Interface.Name) == "" || strings.TrimSpace(m.Interface.Description) == "" || strings.TrimSpace(m.Interface.ArtifactVersion) == "" {
		return errors.New("connector protocol: interface name, description, and artifact version are required")
	}
	if len(m.Interface.Name) > 256 || len(m.Interface.Description) > 4096 || len(m.Interface.ArtifactVersion) > 256 || len(m.Interface.Commands) > 256 || len(m.Interface.Directories) > 256 || len(m.Settings) > 256 || len(m.Features) > 64 {
		return errors.New("connector protocol: manifest metadata or collection exceeds its limit")
	}
	if err := validateDescriptors(m.Interface.Commands, m.Interface.Directories, true); err != nil {
		return err
	}
	seenTargets := make(map[string]bool)
	for _, targetID := range m.Targets {
		target, ok := LookupTarget(targetID)
		if !ok {
			return fmt.Errorf("connector protocol: unsupported target %q", targetID)
		}
		if seenTargets[targetID] {
			return fmt.Errorf("connector protocol: duplicate target %q", targetID)
		}
		seenTargets[targetID] = true
		if !target.SupportsServiceMode(m.ServiceMode) {
			return fmt.Errorf("connector protocol: target %q does not support %s service mode", targetID, m.ServiceMode)
		}
	}
	if len(m.Targets) == 0 {
		return errors.New("connector protocol: at least one target is required")
	}
	seenFeatures := make(map[string]bool, len(m.Features))
	for _, feature := range m.Features {
		if len(feature) > 63 || !kindPattern.MatchString(feature) || seenFeatures[feature] {
			return fmt.Errorf("connector protocol: invalid or duplicate feature %q", feature)
		}
		seenFeatures[feature] = true
	}
	if err := ValidateSettings(m.Settings); err != nil {
		return err
	}
	digest, err := InterfaceDigest(m.Interface)
	if err != nil {
		return err
	}
	if m.InterfaceHash != digest {
		return fmt.Errorf("connector protocol: interface hash is %q, want %q", m.InterfaceHash, digest)
	}
	return nil
}

func validateDescriptors(commands []CommandDescriptor, directories []DirectoryDescriptor, schemas bool) error {
	seen := make(map[string]string)
	for _, command := range commands {
		if err := ValidateName("command", command.Name); err != nil {
			return err
		}
		if previous := seen[command.Name]; previous != "" {
			return fmt.Errorf("connector protocol: duplicate operation %q", command.Name)
		}
		seen[command.Name] = "command"
		if command.Revision < 1 || (command.Mode != CommandModeUnary && command.Mode != CommandModeJob) {
			return fmt.Errorf("connector protocol: command %q has invalid revision or mode", command.Name)
		}
		if len(command.Description) > 4096 || len(command.InputSchema) > MaxSchemaBytes || len(command.OutputSchema) > MaxSchemaBytes {
			return fmt.Errorf("connector protocol: command %q metadata exceeds its limit", command.Name)
		}
		if schemas {
			inputHash, err := HashJSON(command.InputSchema)
			if err != nil || inputHash != command.InputSchemaHash {
				return fmt.Errorf("connector protocol: command %q input schema hash mismatch", command.Name)
			}
			outputHash, err := HashJSON(command.OutputSchema)
			if err != nil || outputHash != command.OutputSchemaHash {
				return fmt.Errorf("connector protocol: command %q output schema hash mismatch", command.Name)
			}
		}
	}
	for _, directory := range directories {
		if err := ValidateName("directory", directory.Name); err != nil {
			return err
		}
		if previous := seen[directory.Name]; previous != "" {
			return fmt.Errorf("connector protocol: duplicate operation %q", directory.Name)
		}
		seen[directory.Name] = "directory"
		if directory.Revision < 1 || (!directory.Read && !directory.Write && !directory.List) {
			return fmt.Errorf("connector protocol: directory %q has invalid revision or access", directory.Name)
		}
		if len(directory.Description) > 4096 {
			return fmt.Errorf("connector protocol: directory %q description exceeds its limit", directory.Name)
		}
	}
	return nil
}

func ValidateSettings(settings []SettingDescriptor) error {
	seen := make(map[string]bool, len(settings))
	for _, setting := range settings {
		if len(setting.Name) > 63 || !kindPattern.MatchString(setting.Name) || seen[setting.Name] || reservedSettingNames[setting.Name] || reservedSettingNames[strings.TrimSuffix(setting.Name, "-file")] || reservedSettingNames[strings.TrimSuffix(setting.Name, "-stdin")] {
			return fmt.Errorf("connector protocol: invalid, duplicate, or reserved setting name %q", setting.Name)
		}
		seen[setting.Name] = true
		if len(setting.Description) > 4096 || len(setting.Default) > 4096 {
			return fmt.Errorf("connector protocol: setting %q metadata exceeds its limit", setting.Name)
		}
		switch setting.Kind {
		case "secret":
			if setting.Default != "" {
				return fmt.Errorf("connector protocol: secret setting %q cannot have a default", setting.Name)
			}
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
		case "string", "file", "directory":
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
		case "url":
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
			if setting.Default != "" {
				parsed, err := url.ParseRequestURI(setting.Default)
				if err != nil || parsed.Scheme == "" || parsed.Host == "" {
					return fmt.Errorf("connector protocol: setting %q has an invalid URL default", setting.Name)
				}
			}
		case "bool":
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
			if setting.Default != "" {
				if _, err := strconv.ParseBool(setting.Default); err != nil {
					return fmt.Errorf("connector protocol: setting %q has an invalid boolean default", setting.Name)
				}
			}
		case "integer":
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
			if setting.Default != "" {
				if _, err := strconv.ParseInt(setting.Default, 10, 64); err != nil {
					return fmt.Errorf("connector protocol: setting %q has an invalid integer default", setting.Name)
				}
			}
		case "duration":
			if len(setting.Enum) != 0 {
				return fmt.Errorf("connector protocol: setting %q cannot have enum values", setting.Name)
			}
			if setting.Default != "" {
				if _, err := time.ParseDuration(setting.Default); err != nil {
					return fmt.Errorf("connector protocol: setting %q has an invalid duration default", setting.Name)
				}
			}
		case "enum":
			if len(setting.Enum) == 0 || len(setting.Enum) > 100 {
				return fmt.Errorf("connector protocol: enum setting %q must have 1 to 100 values", setting.Name)
			}
			values := make(map[string]bool, len(setting.Enum))
			defaultFound := setting.Default == ""
			for _, value := range setting.Enum {
				if value == "" || len(value) > 256 || values[value] {
					return fmt.Errorf("connector protocol: enum setting %q has an invalid or duplicate value", setting.Name)
				}
				values[value] = true
				defaultFound = defaultFound || value == setting.Default
			}
			if !defaultFound {
				return fmt.Errorf("connector protocol: enum setting %q default is not an enum value", setting.Name)
			}
		default:
			return fmt.Errorf("connector protocol: setting %q has unsupported kind %q", setting.Name, setting.Kind)
		}
	}
	for _, setting := range settings {
		if setting.Kind == "secret" {
			if seen[setting.Name+"-file"] || seen[setting.Name+"-stdin"] {
				return fmt.Errorf("connector protocol: setting %q conflicts with generated secret flags", setting.Name)
			}
		}
	}
	return nil
}
