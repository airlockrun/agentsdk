package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

const stateVersion = 1

const rollbackStateVersion = 1

type installationState struct {
	Version        int                `json:"version"`
	ServiceMode    ServiceMode        `json:"serviceMode"`
	AirlockURL     string             `json:"airlockUrl,omitempty"`
	InstallationID string             `json:"installationId,omitempty"`
	Credential     string             `json:"credential,omitempty"`
	StorageOrigins []string           `json:"storageOrigins,omitempty"`
	Pending        *pendingActivation `json:"pending,omitempty"`
	Enabled        bool               `json:"enabled"`
}

type pendingActivation struct {
	DeviceSecret    string              `json:"deviceSecret"`
	UserCode        string              `json:"userCode"`
	VerificationURL string              `json:"verificationUrl"`
	ExpiresAt       time.Time           `json:"expiresAt"`
	PollInterval    int                 `json:"pollIntervalSeconds"`
	StorageOrigins  []string            `json:"storageOrigins,omitempty"`
	Approved        *approvedActivation `json:"approved,omitempty"`
}

type approvedActivation struct {
	InstallationID string   `json:"installationId"`
	Credential     string   `json:"credential"`
	StorageOrigins []string `json:"storageOrigins,omitempty"`
}

type runtimeStatus struct {
	Version         int                `json:"version"`
	Readiness       protocol.Readiness `json:"readiness"`
	ArtifactVersion string             `json:"artifactVersion"`
	ArtifactDigest  string             `json:"artifactDigest"`
	Message         string             `json:"message,omitempty"`
	UpdatedAt       time.Time          `json:"updatedAt"`
}

type rollbackState struct {
	Version         int         `json:"version"`
	ServiceMode     ServiceMode `json:"serviceMode"`
	InstallationID  string      `json:"installationId"`
	Installation    []byte      `json:"installation"`
	Settings        []byte      `json:"settings,omitempty"`
	SettingsPresent bool        `json:"settingsPresent"`
	SettingsSchema  []byte      `json:"settingsSchema"`
}

func saveRollbackState(path string, state rollbackState) error {
	if state.ServiceMode != ServiceSystem && state.ServiceMode != ServiceUser {
		return errors.New("connector: rollback state service mode is required")
	}
	if len(state.Installation) == 0 || len(state.SettingsSchema) == 0 {
		return errors.New("connector: rollback installation and settings schema state are required")
	}
	state.Version = rollbackStateVersion
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, 0o600)
}

func loadRollbackState(path string) (rollbackState, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return rollbackState{}, err
	}
	var state rollbackState
	if err := strictUnmarshal(body, &state); err != nil {
		return rollbackState{}, fmt.Errorf("connector: decode rollback state: %w", err)
	}
	if state.Version != rollbackStateVersion || (state.ServiceMode != ServiceSystem && state.ServiceMode != ServiceUser) || len(state.Installation) == 0 || state.SettingsPresent != (state.Settings != nil) || len(state.SettingsSchema) == 0 {
		return rollbackState{}, errors.New("connector: invalid rollback state")
	}
	return state, nil
}

func saveRuntimeStatus(path string, status runtimeStatus) error {
	if err := protocol.ValidateArtifactDigest(status.ArtifactDigest); err != nil {
		return err
	}
	status.Version, status.UpdatedAt = 1, time.Now().UTC()
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, 0o600)
}

func loadRuntimeStatus(path string) (runtimeStatus, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return runtimeStatus{}, err
	}
	var status runtimeStatus
	if err := strictUnmarshal(body, &status); err != nil {
		return status, err
	}
	if status.Version != 1 {
		return status, fmt.Errorf("connector: unsupported runtime status version %d", status.Version)
	}
	if err := protocol.ValidateArtifactDigest(status.ArtifactDigest); err != nil {
		return runtimeStatus{}, err
	}
	return status, nil
}

func installationDirectories(base string) ([]string, error) {
	root := filepath.Join(base, "installations")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !validInstallationID(entry.Name()) {
			return nil, fmt.Errorf("connector: invalid installation state entry %q", entry.Name())
		}
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

func validInstallationID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func loadInstallation(path string, machine bool) (installationState, error) {
	protected, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return installationState{Version: stateVersion, Enabled: true}, nil
	}
	if err != nil {
		return installationState{}, err
	}
	body, err := unprotectBytes(protected, machine)
	if err != nil {
		return installationState{}, fmt.Errorf("connector: unprotect installation state: %w", err)
	}
	var state installationState
	if err := strictUnmarshal(body, &state); err != nil {
		return installationState{}, fmt.Errorf("connector: decode installation state: %w", err)
	}
	if state.Version != stateVersion {
		return installationState{}, fmt.Errorf("connector: unsupported state version %d", state.Version)
	}
	if state.ServiceMode != ServiceSystem && state.ServiceMode != ServiceUser {
		return installationState{}, errors.New("connector: persisted installation state has no valid service mode")
	}
	return state, nil
}

func saveInstallation(path string, state installationState, machine bool) error {
	if state.ServiceMode != ServiceSystem && state.ServiceMode != ServiceUser {
		return errors.New("connector: installation service mode is required")
	}
	state.Version = stateVersion
	state.StorageOrigins = canonicalOrigins(state.StorageOrigins)
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	protected, err := protectBytes(body, machine)
	if err != nil {
		return fmt.Errorf("connector: protect installation state: %w", err)
	}
	return atomicWrite(path, protected, 0o600)
}

func loadSettings(path string, settings any, machine bool) error {
	if settings == nil {
		return nil
	}
	body, err := readSettings(path, machine)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := strictUnmarshal(body, settings); err != nil {
		return fmt.Errorf("connector: decode settings: %w", err)
	}
	return nil
}

func readSettings(path string, machine bool) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body, err = unprotectBytes(body, machine)
	if err != nil {
		return nil, fmt.Errorf("connector: unprotect settings: %w", err)
	}
	return body, nil
}

func saveSettings(path string, settings any, machine bool) error {
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	body, err = protectBytes(body, machine)
	if err != nil {
		return fmt.Errorf("connector: protect settings: %w", err)
	}
	return atomicWrite(path, body, 0o600)
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func ensurePrivateDirectory(path string) error {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	if err := syncDirectory(path); err != nil {
		return err
	}
	if !existed {
		return syncDirectory(filepath.Dir(path))
	}
	return nil
}

func canonicalOrigins(origins []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(origins))
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			continue
		}
		value := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
