package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

func (r *Runtime) activate(ctx context.Context, args []string) error {
	set := flag.NewFlagSet("activate", flag.ContinueOnError)
	set.SetOutput(r.config.ErrorOutput)
	airlock := set.String("airlock", "", "Airlock HTTPS origin")
	noBrowser := set.Bool("no-browser", false, "do not open a browser")
	noWait := set.Bool("no-wait", false, "save pending activation and exit")
	wait := set.Bool("wait", false, "wait even without a TTY")
	check := set.Bool("check", false, "check pending activation once")
	newInstallation := set.Bool("new", false, "activate another installation")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("connector: activate takes no positional arguments")
	}
	if *noWait && (*wait || *check) || *wait && *check || *check && *noBrowser {
		return errors.New("connector: --check cannot be combined with --no-browser, --no-wait, or --wait; --no-wait and --wait are mutually exclusive")
	}
	if *check && *newInstallation {
		return errors.New("connector: --check cannot be combined with --new")
	}
	if *newInstallation {
		r.stateDir, r.installationID, r.ambiguousInstallations = draftStateDirectory(r.stateBase), "", false
		if err := prepareStateDirectory(r.stateDir, r.config.ServiceMode, r.config.Operations); err != nil {
			return err
		}
	}
	if *check && r.installationID != "" {
		draftDir := draftStateDirectory(r.stateBase)
		lock, err := acquireFileLock(ctx, filepath.Join(draftDir, ".installation.lock"))
		if err != nil {
			return err
		}
		pending, loadErr := r.loadInstallation(filepath.Join(draftDir, "installation.json"))
		closeErr := lock.Close()
		if loadErr != nil {
			return loadErr
		}
		if closeErr != nil {
			return closeErr
		}
		if pending.Pending != nil {
			r.stateDir, r.installationID = draftDir, ""
		}
	}
	lock, err := acquireFileLock(ctx, r.installationLockPath())
	if err != nil {
		return fmt.Errorf("connector: acquire installation process lock: %w", err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = lock.Close()
		}
	}()
	releaseLock := func() error {
		if !lockHeld {
			return nil
		}
		lockHeld = false
		return lock.Close()
	}
	if r.config.Settings != nil {
		if err := json.Unmarshal(r.initialSettings, r.config.Settings); err != nil {
			return err
		}
	}
	if err := loadSettings(filepath.Join(r.stateDir, "settings.json"), r.config.Settings, r.machineState); err != nil {
		return err
	}
	statePath := filepath.Join(r.stateDir, "installation.json")
	state, err := r.loadInstallation(statePath)
	if err != nil {
		return err
	}
	if *airlock == "" {
		*airlock = state.AirlockURL
	}
	if *airlock == "" {
		return errors.New("connector: activate requires --airlock")
	}
	baseURL, err := validateAirlockOrigin(*airlock, r.config.AllowInsecureHTTP)
	if err != nil {
		return err
	}
	if err := saveInstallation(statePath, state, r.machineState); err != nil {
		return err
	}
	if err := r.validateForMode(ctx); err != nil {
		return err
	}
	if *check {
		if state.Pending == nil {
			return errors.New("connector: no pending activation")
		}
		if state.AirlockURL != baseURL {
			return errors.New("connector: --airlock does not match the pending activation")
		}
		if err := releaseLock(); err != nil {
			return err
		}
		return r.pollActivation(ctx, statePath, false)
	}
	var response protocol.DeviceCodeResponse
	if err := r.enrollmentPost(ctx, baseURL, "/api/connectors/v1/enroll/device-code", protocol.DeviceCodeRequest{Manifest: r.Manifest()}, &response); err != nil {
		return err
	}
	if len(response.DeviceSecret) < 32 || len(response.DeviceSecret) > 128 || response.UserCode == "" || response.VerificationURL == "" || !response.ExpiresAt.After(time.Now()) || response.PollIntervalSeconds < 1 {
		return errors.New("connector: Airlock returned an invalid device activation")
	}
	verification, err := url.Parse(response.VerificationURL)
	if err != nil || strings.ToLower(verification.Scheme)+"://"+strings.ToLower(verification.Host) != baseURL || verification.User != nil {
		return errors.New("connector: activation verification URL must use the exact Airlock origin")
	}
	origins, err := validateStorageOrigins(response.StorageOrigins)
	if err != nil {
		return err
	}
	state.AirlockURL = baseURL
	state.Pending = &pendingActivation{DeviceSecret: response.DeviceSecret, UserCode: response.UserCode, VerificationURL: response.VerificationURL, ExpiresAt: response.ExpiresAt, PollInterval: response.PollIntervalSeconds, StorageOrigins: origins}
	if err := saveInstallation(statePath, state, r.machineState); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.config.Output, "Open: %s\nCode: %s\n", response.VerificationURL, response.UserCode)
	for _, origin := range origins {
		_, _ = fmt.Fprintf(r.config.Output, "Storage origin requiring approval: %s\n", origin)
	}
	interactive := isTerminal(r.config.Input)
	shouldWait := *wait || (interactive && !*noWait)
	if !*noBrowser && interactive {
		_ = openBrowser(response.VerificationURL)
	}
	if !shouldWait {
		executable, err := r.config.Operations.Executable()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(r.config.Output, "Activation is pending. Check once with:\n  %q activate --airlock %s --check\n", executable, baseURL)
		return err
	}
	if err := releaseLock(); err != nil {
		return err
	}
	return r.pollActivation(ctx, statePath, true)
}

func (r *Runtime) pollActivation(ctx context.Context, statePath string, wait bool) error {
	for {
		lock, err := acquireFileLock(ctx, filepath.Join(filepath.Dir(statePath), ".installation.lock"))
		if err != nil {
			return err
		}
		state, err := r.loadInstallation(statePath)
		if err != nil {
			_ = lock.Close()
			return err
		}
		if state.Pending == nil {
			_ = lock.Close()
			return errors.New("connector: no pending activation")
		}
		if state.Pending.Approved != nil {
			err := r.completeActivation(statePath, state)
			return errors.Join(err, lock.Close())
		}
		if !state.Pending.ExpiresAt.After(time.Now()) {
			state.Pending = nil
			saveErr := saveInstallation(statePath, state, r.machineState)
			_ = lock.Close()
			if saveErr != nil {
				return saveErr
			}
			return errors.New("connector: pending activation expired")
		}
		deviceSecret := state.Pending.DeviceSecret
		pollInterval := state.Pending.PollInterval
		if err := lock.Close(); err != nil {
			return err
		}
		var response protocol.EnrollmentResponse
		err = r.enrollmentPost(ctx, state.AirlockURL, "/api/connectors/v1/enroll/complete", protocol.EnrollmentRequest{DeviceSecret: deviceSecret}, &response)
		if err != nil {
			return err
		}
		switch response.Status {
		case "approved":
			if response.InstallationID == "" || len(response.Credential) < 32 {
				return errors.New("connector: approved activation omitted installation credentials")
			}
			origins, err := validateStorageOrigins(response.StorageOrigins)
			if err != nil {
				return err
			}
			if !validInstallationID(response.InstallationID) {
				return errors.New("connector: Airlock returned an invalid installation ID")
			}
			return r.commitActivationApproval(ctx, statePath, deviceSecret, response, origins)
		case "pending":
			if !wait {
				return errors.New("connector: activation is still pending")
			}
		case "denied":
			if err := r.clearPendingActivation(ctx, statePath, deviceSecret); err != nil {
				return err
			}
			return errors.New("connector: activation was denied")
		case "expired":
			if err := r.clearPendingActivation(ctx, statePath, deviceSecret); err != nil {
				return err
			}
			return errors.New("connector: activation expired")
		default:
			return fmt.Errorf("connector: unknown activation status %q", response.Status)
		}
		timer := time.NewTimer(time.Duration(pollInterval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runtime) commitActivationApproval(ctx context.Context, statePath, deviceSecret string, response protocol.EnrollmentResponse, origins []string) error {
	lock, err := acquireFileLock(ctx, filepath.Join(filepath.Dir(statePath), ".installation.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := r.loadInstallation(statePath)
	if err != nil {
		return err
	}
	if state.Pending == nil || state.Pending.DeviceSecret != deviceSecret {
		return errors.New("connector: pending activation changed while polling")
	}
	if !equalStrings(origins, state.Pending.StorageOrigins) {
		return errors.New("connector: approved storage origins differ from the locally confirmed activation origins")
	}
	if r.installationID != "" && r.installationID != response.InstallationID {
		return errors.New("connector: activation completion changed the selected installation ID")
	}
	state.Pending.Approved = &approvedActivation{InstallationID: response.InstallationID, Credential: response.Credential, StorageOrigins: origins}
	if err := saveInstallation(statePath, state, r.machineState); err != nil {
		return fmt.Errorf("connector: reserve approved activation locally: %w", err)
	}
	return r.completeActivation(statePath, state)
}

func (r *Runtime) clearPendingActivation(ctx context.Context, statePath, deviceSecret string) error {
	lock, err := acquireFileLock(ctx, filepath.Join(filepath.Dir(statePath), ".installation.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := r.loadInstallation(statePath)
	if err != nil {
		return err
	}
	if state.Pending == nil || state.Pending.DeviceSecret != deviceSecret {
		return errors.New("connector: pending activation changed while polling")
	}
	state.Pending = nil
	return saveInstallation(statePath, state, r.machineState)
}

func (r *Runtime) completeActivation(statePath string, state installationState) error {
	approved := state.Pending.Approved
	state.InstallationID, state.Credential = approved.InstallationID, approved.Credential
	state.StorageOrigins = append([]string(nil), approved.StorageOrigins...)
	targetDir := filepath.Join(r.stateBase, "installations", approved.InstallationID)
	targetStatePath := filepath.Join(targetDir, "installation.json")
	if r.installationID == "" && filepath.Clean(statePath) != filepath.Clean(targetStatePath) {
		if _, err := os.Lstat(targetDir); err == nil {
			if _, statErr := os.Stat(targetStatePath); statErr == nil {
				existing, loadErr := r.loadInstallation(targetStatePath)
				if loadErr != nil || existing.InstallationID != approved.InstallationID || existing.Credential != approved.Credential {
					return errors.New("connector: installation state already exists")
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := prepareStateDirectory(targetDir, r.config.ServiceMode, r.config.Operations); err != nil {
		return err
	}
	state.Pending = nil
	if err := saveInstallation(targetStatePath, state, r.machineState); err != nil {
		return err
	}
	if r.config.Settings != nil {
		if err := saveSettings(filepath.Join(targetDir, "settings.json"), r.config.Settings, r.machineState); err != nil {
			return err
		}
	}
	if err := saveSettingsSchema(filepath.Join(targetDir, "settings-schema.json"), r.settings, r.settingFields); err != nil {
		return err
	}
	if filepath.Clean(statePath) != filepath.Clean(targetStatePath) {
		_ = os.Remove(statePath)
		_ = os.Remove(filepath.Join(filepath.Dir(statePath), "settings.json"))
		_ = os.Remove(filepath.Join(filepath.Dir(statePath), "settings-schema.json"))
	}
	r.stateDir, r.installationID = targetDir, approved.InstallationID
	_, err := fmt.Fprintln(r.config.Output, "Connector activated.")
	return err
}

func validateStorageOrigins(values []string) ([]string, error) {
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("connector: invalid approved storage origin %q", value)
		}
	}
	return canonicalOrigins(values), nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *Runtime) enrollmentPost(ctx context.Context, baseURL, endpoint string, request, result any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := noRedirectClient(r.config.HTTPClient).Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("connector: activation HTTP %d: %s", response.StatusCode, data)
	}
	return decodeBounded(response.Body, result, maximumProtocolBody)
}

func (r *Runtime) unregister(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("connector: unregister takes no arguments")
	}
	statePath := filepath.Join(r.stateDir, "installation.json")
	state, err := r.loadInstallation(statePath)
	if err != nil {
		return err
	}
	if state.Credential == "" {
		return errors.New("connector: installation is not activated")
	}
	client, err := newProtocolClient(state.AirlockURL, state.Credential, r.config.HTTPClient, r.config.AllowInsecureHTTP)
	if err != nil {
		return err
	}
	if err := client.post(ctx, "/api/connectors/v1/unregister", struct{}{}, nil); err != nil {
		return err
	}
	state.Credential, state.InstallationID, state.AirlockURL, state.StorageOrigins, state.Pending = "", "", "", nil, nil
	return saveInstallation(statePath, state, r.machineState)
}

func validateAirlockOrigin(raw string, insecure bool) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("connector: Airlock URL must be an origin")
	}
	if parsed.Scheme != "https" && !(insecure && parsed.Scheme == "http") {
		return "", errors.New("connector: Airlock URL must use HTTPS")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}
