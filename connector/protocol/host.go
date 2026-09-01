package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	HostProtocolVersion           = 1
	MaxChildFrameBytes            = 8 << 20
	MaxHostSyncBytes              = MaxChildFrameBytes
	MaxHostInventoryMutationBytes = 2*MaxManifestBytes + (64 << 10)
	MaxHostedConnectors           = 1000
	MaxHostedStatusErrorBytes     = 128
	MaxHostDisplayNameBytes       = 256
	MaxHostStorageOrigins         = 64
	MaxHostStorageOriginBytes     = 2048
	maxHostNameBytes              = 256
	maxHostVersionBytes           = 256
)

type RemoteAccessMode string

const (
	RemoteAccessFull       RemoteAccessMode = "full"
	RemoteAccessUpdateOnly RemoteAccessMode = "update_only"
	RemoteAccessNone       RemoteAccessMode = "none"
)

type HostInfo struct {
	ProtocolVersion int              `json:"protocolVersion"`
	Name            string           `json:"name"`
	Platform        string           `json:"platform"`
	Architecture    string           `json:"architecture"`
	AccessMode      RemoteAccessMode `json:"accessMode"`
	Version         string           `json:"version"`
}

type HostedConnectorStatus struct {
	InstallationID string                  `json:"installationId"`
	Manifest       HostedConnectorManifest `json:"manifest"`
	Readiness      Readiness               `json:"readiness"`
	ActiveAttempts []ActiveAttempt         `json:"activeAttempts,omitempty"`
	Error          string                  `json:"error,omitempty"`
}

type HostedConnectorManifest struct {
	ProtocolMajor   int      `json:"protocolMajor"`
	ProtocolMinor   int      `json:"protocolMinor"`
	Features        []string `json:"features"`
	ArtifactVersion string   `json:"artifactVersion"`
	ArtifactDigest  string   `json:"artifactDigest"`
	InterfaceHash   string   `json:"interfaceHash"`
}

func SummarizeManifest(manifest Manifest) HostedConnectorManifest {
	return HostedConnectorManifest{
		ProtocolMajor: manifest.ProtocolMajor, ProtocolMinor: manifest.ProtocolMinor,
		Features: append([]string(nil), manifest.Features...), ArtifactVersion: manifest.Interface.ArtifactVersion,
		ArtifactDigest: manifest.ArtifactDigest, InterfaceHash: manifest.InterfaceHash,
	}
}

func ValidateHostedConnectorManifest(manifest HostedConnectorManifest) error {
	if manifest.ProtocolMajor != Major || manifest.ProtocolMinor < 0 || manifest.ProtocolMinor > math.MaxInt32 || strings.TrimSpace(manifest.ArtifactVersion) == "" || len(manifest.ArtifactVersion) > 256 || len(manifest.Features) > 64 {
		return errors.New("connector protocol: invalid hosted manifest summary")
	}
	if err := ValidateArtifactDigest(manifest.ArtifactDigest); err != nil {
		return err
	}
	if err := ValidateArtifactDigest(manifest.InterfaceHash); err != nil {
		return errors.New("connector protocol: invalid hosted interface hash")
	}
	seen := make(map[string]bool, len(manifest.Features))
	for _, feature := range manifest.Features {
		if len(feature) > 63 || !kindPattern.MatchString(feature) || seen[feature] {
			return fmt.Errorf("connector protocol: invalid or duplicate hosted feature %q", feature)
		}
		seen[feature] = true
	}
	return nil
}

type HostSyncRequest struct {
	Host       HostInfo                `json:"host"`
	Connectors []HostedConnectorStatus `json:"connectors"`
}

type HostSyncResponse struct {
	HostID           string `json:"hostId"`
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
	LongPollSeconds  int    `json:"longPollSeconds"`
}

type HostConnectorMutationKind string

const (
	HostConnectorMutationUpsert HostConnectorMutationKind = "upsert"
	HostConnectorMutationRemove HostConnectorMutationKind = "remove"
)

// ObservedConnectorArtifact binds a validated manifest to the digest measured
// from the executable bytes by airlock-host.
type ObservedConnectorArtifact struct {
	Manifest       Manifest `json:"manifest"`
	MeasuredDigest string   `json:"measuredDigest"`
}

// HostConnectorInventoryMutationRequest is one durable, monotonic mutation for
// a physical connector installation. A remove mutation is a tombstone.
type HostConnectorInventoryMutationRequest struct {
	InstallationID string                     `json:"installationId"`
	Revision       uint64                     `json:"revision"`
	Kind           HostConnectorMutationKind  `json:"kind"`
	DisplayName    string                     `json:"displayName,omitempty"`
	Active         *ObservedConnectorArtifact `json:"active,omitempty"`
	Rollback       *ObservedConnectorArtifact `json:"rollback,omitempty"`
}

// HostConnectorInventoryMutationResponse acknowledges the durable revision and
// returns the storage origins Airlock authorizes for the active installation.
type HostConnectorInventoryMutationResponse struct {
	InstallationID       string   `json:"installationId"`
	AcknowledgedRevision uint64   `json:"acknowledgedRevision"`
	StorageOrigins       []string `json:"storageOrigins,omitempty"`
}

func ValidateHostSyncRequest(request HostSyncRequest) error {
	if request.Host.ProtocolVersion != HostProtocolVersion {
		return fmt.Errorf("connector protocol: unsupported host protocol version %d", request.Host.ProtocolVersion)
	}
	if strings.TrimSpace(request.Host.Name) == "" || len(request.Host.Name) > maxHostNameBytes || strings.TrimSpace(request.Host.Version) == "" || len(request.Host.Version) > maxHostVersionBytes {
		return errors.New("connector protocol: invalid host name or version")
	}
	if _, ok := LookupTarget(request.Host.Platform + "-" + request.Host.Architecture); !ok {
		return errors.New("connector protocol: unsupported host platform and architecture")
	}
	switch request.Host.AccessMode {
	case RemoteAccessFull, RemoteAccessUpdateOnly, RemoteAccessNone:
	default:
		return fmt.Errorf("connector protocol: invalid remote access mode %q", request.Host.AccessMode)
	}
	if len(request.Connectors) > MaxHostedConnectors {
		return fmt.Errorf("connector protocol: host sync exceeds %d connectors", MaxHostedConnectors)
	}
	seen := make(map[string]bool, len(request.Connectors))
	for _, connector := range request.Connectors {
		if err := validateInstallationID(connector.InstallationID); err != nil {
			return err
		}
		if seen[connector.InstallationID] {
			return fmt.Errorf("connector protocol: duplicate installation ID %q", connector.InstallationID)
		}
		seen[connector.InstallationID] = true
		if err := ValidateHostedConnectorManifest(connector.Manifest); err != nil {
			return err
		}
		if !validReadiness(connector.Readiness) {
			return fmt.Errorf("connector protocol: invalid hosted connector readiness %q", connector.Readiness)
		}
		if len(connector.ActiveAttempts) > MaxActiveAttempts {
			return fmt.Errorf("connector protocol: installation %q exceeds %d active attempts", connector.InstallationID, MaxActiveAttempts)
		}
		if len(connector.Error) > MaxHostedStatusErrorBytes {
			return fmt.Errorf("connector protocol: installation %q status error exceeds its limit", connector.InstallationID)
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(body) > MaxHostSyncBytes {
		return fmt.Errorf("connector protocol: host sync exceeds %d bytes", MaxHostSyncBytes)
	}
	return nil
}

func ValidateHostConnectorInventoryMutationRequest(request HostConnectorInventoryMutationRequest) error {
	if err := validateInstallationID(request.InstallationID); err != nil {
		return err
	}
	if err := validateInventoryRevision(request.Revision); err != nil {
		return err
	}
	switch request.Kind {
	case HostConnectorMutationUpsert:
		if strings.TrimSpace(request.DisplayName) == "" || len(request.DisplayName) > MaxHostDisplayNameBytes {
			return errors.New("connector protocol: inventory upsert has an invalid display name")
		}
		if request.Active == nil {
			return errors.New("connector protocol: inventory upsert requires an active artifact")
		}
		if err := validateObservedConnectorArtifact("active", *request.Active); err != nil {
			return err
		}
		if request.Rollback != nil {
			if err := validateObservedConnectorArtifact("rollback", *request.Rollback); err != nil {
				return err
			}
		}
	case HostConnectorMutationRemove:
		if request.DisplayName != "" || request.Active != nil || request.Rollback != nil {
			return errors.New("connector protocol: inventory removal tombstone cannot carry artifact fields")
		}
	default:
		return fmt.Errorf("connector protocol: invalid inventory mutation kind %q", request.Kind)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(body) > MaxHostInventoryMutationBytes {
		return fmt.Errorf("connector protocol: inventory mutation exceeds %d bytes", MaxHostInventoryMutationBytes)
	}
	return nil
}

func ValidateHostConnectorInventoryMutationResponse(response HostConnectorInventoryMutationResponse) error {
	if err := validateInstallationID(response.InstallationID); err != nil {
		return err
	}
	if err := validateInventoryRevision(response.AcknowledgedRevision); err != nil {
		return err
	}
	return validateHostStorageOrigins(response.StorageOrigins)
}

func validateInventoryRevision(revision uint64) error {
	if revision == 0 || revision > math.MaxInt64 {
		return errors.New("connector protocol: inventory revision must be between 1 and math.MaxInt64")
	}
	return nil
}

func validateObservedConnectorArtifact(slot string, artifact ObservedConnectorArtifact) error {
	if err := ValidateManifest(artifact.Manifest); err != nil {
		return fmt.Errorf("connector protocol: invalid %s manifest: %w", slot, err)
	}
	manifestJSON, err := json.Marshal(artifact.Manifest)
	if err != nil {
		return err
	}
	if len(manifestJSON) > MaxManifestBytes {
		return fmt.Errorf("connector protocol: %s manifest exceeds %d bytes", slot, MaxManifestBytes)
	}
	if err := ValidateArtifactDigest(artifact.MeasuredDigest); err != nil {
		return fmt.Errorf("connector protocol: invalid %s measured digest: %w", slot, err)
	}
	if artifact.MeasuredDigest != artifact.Manifest.ArtifactDigest {
		return fmt.Errorf("connector protocol: %s measured digest does not match its manifest", slot)
	}
	return nil
}

func validateInstallationID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return errors.New("connector protocol: installation ID must be a canonical non-zero UUID")
	}
	return nil
}

func validReadiness(value Readiness) bool {
	switch value {
	case ReadinessOffline, ReadinessStarting, ReadinessNeedsConfiguration, ReadinessIncompatible, ReadinessUnhealthy, ReadinessReady:
		return true
	default:
		return false
	}
}

func validateHostStorageOrigins(origins []string) error {
	if len(origins) > MaxHostStorageOrigins {
		return fmt.Errorf("connector protocol: response exceeds %d storage origins", MaxHostStorageOrigins)
	}
	seen := make(map[string]bool, len(origins))
	for _, origin := range origins {
		parsed, err := url.ParseRequestURI(origin)
		canonical := ""
		if err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" {
			canonical = "https://" + strings.ToLower(parsed.Host)
		}
		if len(origin) > MaxHostStorageOriginBytes || canonical == "" || origin != canonical || seen[origin] {
			return fmt.Errorf("connector protocol: invalid or duplicate Airlock-authorized storage origin %q", origin)
		}
		seen[origin] = true
	}
	return nil
}

type HostPollRequest struct {
	ActiveManagementAttempts []ActiveAttempt `json:"activeManagementAttempts,omitempty"`
	ActiveConnectorAttempts  []ActiveAttempt `json:"activeConnectorAttempts,omitempty"`
}

type HostPollResponse struct {
	Work []HostWork `json:"work"`
}

type HostWorkKind string

const (
	HostWorkConnectorJob      HostWorkKind = "connector_job"
	HostWorkConnectorCancel   HostWorkKind = "connector_cancel"
	HostWorkShell             HostWorkKind = "shell"
	HostWorkConnectorInstall  HostWorkKind = "connector_install"
	HostWorkConnectorUpdate   HostWorkKind = "connector_update"
	HostWorkConnectorRemove   HostWorkKind = "connector_remove"
	HostWorkConnectorRollback HostWorkKind = "connector_rollback"
)

type HostWork struct {
	Kind          HostWorkKind       `json:"kind"`
	ConnectorID   string             `json:"connectorId,omitempty"`
	ConnectorJob  *JobRequest        `json:"connectorJob,omitempty"`
	Cancel        *ChildCancel       `json:"cancel,omitempty"`
	ManagementJob *HostManagementJob `json:"managementJob,omitempty"`
}

type HostManagementJob struct {
	JobID        string          `json:"jobId"`
	AttemptToken string          `json:"attemptToken"`
	Input        json.RawMessage `json:"input"`
	Deadline     time.Time       `json:"deadline"`
}

type HostManagementEvent struct {
	AttemptToken string    `json:"attemptToken"`
	Sequence     int64     `json:"sequence"`
	Phase        string    `json:"phase"`
	Message      string    `json:"message,omitempty"`
	Time         time.Time `json:"time"`
}

type HostManagementCompletion struct {
	JobID        string          `json:"jobId"`
	AttemptToken string          `json:"attemptToken"`
	Status       string          `json:"status"`
	Output       json.RawMessage `json:"output,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// ChildEnvelope is the framed JSON protocol between airlock-host and one
// connector child. Exactly one payload matching Type must be present.
type ChildEnvelope struct {
	Type       ChildMessageType `json:"type"`
	Initialize *ChildInitialize `json:"initialize,omitempty"`
	Settings   *ChildSettings   `json:"settings,omitempty"`
	Ready      *ChildReady      `json:"ready,omitempty"`
	Job        *JobRequest      `json:"job,omitempty"`
	Cancel     *ChildCancel     `json:"cancel,omitempty"`
	Event      *JobEvent        `json:"event,omitempty"`
	Completion *JobCompletion   `json:"completion,omitempty"`
}

type ChildMessageType string

const (
	ChildMessageInitialize ChildMessageType = "initialize"
	ChildMessageSettings   ChildMessageType = "settings"
	ChildMessageReady      ChildMessageType = "ready"
	ChildMessageJob        ChildMessageType = "job"
	ChildMessageCancel     ChildMessageType = "cancel"
	ChildMessageEvent      ChildMessageType = "event"
	ChildMessageCompletion ChildMessageType = "completion"
)

type ChildInitialize struct {
	ProtocolVersion int             `json:"protocolVersion"`
	InstallationID  string          `json:"installationId"`
	Settings        json.RawMessage `json:"settings"`
	StateDirectory  string          `json:"stateDirectory"`
	StorageOrigins  []string        `json:"storageOrigins,omitempty"`
}

type ChildSettings struct {
	Settings       json.RawMessage `json:"settings"`
	StorageOrigins []string        `json:"storageOrigins,omitempty"`
}

type ChildReady struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Manifest        Manifest  `json:"manifest"`
	Readiness       Readiness `json:"readiness"`
	Error           string    `json:"error,omitempty"`
}

type ChildCancel struct {
	JobID        string `json:"jobId"`
	AttemptToken string `json:"attemptToken"`
}

type ShellInput struct {
	Command          string            `json:"command"`
	Arguments        []string          `json:"arguments,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Stdin            []byte            `json:"stdin,omitempty"`
	MaxOutputBytes   int64             `json:"maxOutputBytes,omitempty"`
}

type ShellOutput struct {
	ExitCode int    `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
}

type ConnectorArtifactInput struct {
	InstallationID string          `json:"installationId,omitempty"`
	URL            string          `json:"url"`
	Filename       string          `json:"filename"`
	SHA256         string          `json:"sha256"`
	SizeBytes      int64           `json:"sizeBytes"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	StorageOrigins []string        `json:"storageOrigins,omitempty"`
}

// ChildEncoder writes concurrency-safe, length-prefixed child protocol frames.
type ChildEncoder struct {
	writer io.Writer
	lock   chan struct{}
}

func NewChildEncoder(writer io.Writer) *ChildEncoder {
	if writer == nil {
		panic("connector protocol: child encoder writer is required")
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &ChildEncoder{writer: writer, lock: lock}
}

func (e *ChildEncoder) Encode(envelope ChildEnvelope) error {
	if err := ValidateChildEnvelope(envelope); err != nil {
		return err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(body) > MaxChildFrameBytes {
		return fmt.Errorf("connector protocol: child frame exceeds %d bytes", MaxChildFrameBytes)
	}
	<-e.lock
	defer func() { e.lock <- struct{}{} }()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := e.writer.Write(header[:]); err != nil {
		return err
	}
	_, err = e.writer.Write(body)
	return err
}

// ChildDecoder reads strict, length-prefixed child protocol frames.
type ChildDecoder struct{ reader *bufio.Reader }

func NewChildDecoder(reader io.Reader) *ChildDecoder {
	if reader == nil {
		panic("connector protocol: child decoder reader is required")
	}
	return &ChildDecoder{reader: bufio.NewReader(reader)}
}

func (d *ChildDecoder) Decode(envelope *ChildEnvelope) error {
	if envelope == nil {
		return errors.New("connector protocol: child envelope is required")
	}
	*envelope = ChildEnvelope{}
	var header [4]byte
	if _, err := io.ReadFull(d.reader, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxChildFrameBytes {
		return fmt.Errorf("connector protocol: invalid child frame size %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(d.reader, body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(envelope); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("connector protocol: child frame has trailing JSON")
	}
	return ValidateChildEnvelope(*envelope)
}

func ValidateChildEnvelope(envelope ChildEnvelope) error {
	payloads := 0
	for _, present := range []bool{envelope.Initialize != nil, envelope.Settings != nil, envelope.Ready != nil, envelope.Job != nil, envelope.Cancel != nil, envelope.Event != nil, envelope.Completion != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("connector protocol: child envelope must contain exactly one payload")
	}
	valid := envelope.Type == ChildMessageInitialize && envelope.Initialize != nil ||
		envelope.Type == ChildMessageSettings && envelope.Settings != nil ||
		envelope.Type == ChildMessageReady && envelope.Ready != nil ||
		envelope.Type == ChildMessageJob && envelope.Job != nil ||
		envelope.Type == ChildMessageCancel && envelope.Cancel != nil ||
		envelope.Type == ChildMessageEvent && envelope.Event != nil ||
		envelope.Type == ChildMessageCompletion && envelope.Completion != nil
	if !valid {
		return fmt.Errorf("connector protocol: child envelope type %q does not match its payload", envelope.Type)
	}
	return nil
}
