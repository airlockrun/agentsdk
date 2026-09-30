package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const credentialsLockTimeout = 30 * time.Second

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "airlock", "credentials.json"), nil
}

func canonicalAuthOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid Airlock URL %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("Airlock URL must start with http:// or https://")
	}
	if u.User != nil || u.Host == "" {
		return "", fmt.Errorf("Airlock URL %q must contain only an http(s) origin", raw)
	}
	if strings.Trim(u.EscapedPath(), "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("Airlock URL %q must not contain a path, query, or fragment", raw)
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("Airlock URL %q has no hostname", raw)
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return (&url.URL{Scheme: scheme, Host: host}).String(), nil
}

func loadCredentials() (credentialsFile, error) {
	var creds credentialsFile
	err := withCredentialsLock(context.Background(), func(path string) error {
		var err error
		creds, err = readCredentialsUnlocked(path)
		if err != nil {
			return err
		}
		changed, err := canonicalizeCredentials(&creds)
		if err != nil {
			return err
		}
		if changed || !credentialFileModeSecure(path) {
			return writeCredentialsUnlocked(path, creds)
		}
		return nil
	})
	return creds, err
}

func mutateCredentials(ctx context.Context, mutate func(*credentialsFile) error) error {
	return withCredentialsLock(ctx, func(path string) error {
		creds, err := readCredentialsUnlocked(path)
		if err != nil {
			return err
		}
		if _, err := canonicalizeCredentials(&creds); err != nil {
			return err
		}
		if err := mutate(&creds); err != nil {
			return err
		}
		return writeCredentialsUnlocked(path, creds)
	})
}

func withCredentialsLock(ctx context.Context, fn func(path string) error) (retErr error) {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credential directory: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, credentialsLockTimeout)
	defer cancel()
	fileLock := flock.New(path + ".lock")
	locked, err := fileLock.TryLockContext(lockCtx, 25*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for credential lock %s: %w", path+".lock", err)
		}
		return fmt.Errorf("lock credentials: %w", err)
	}
	if !locked {
		return fmt.Errorf("credential lock %s was not acquired", path+".lock")
	}
	defer func() {
		if err := fileLock.Unlock(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("unlock credentials: %w", err))
		}
	}()
	return fn(path)
}

func readCredentialsUnlocked(path string) (credentialsFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyCredentials(), nil
		}
		return credentialsFile{}, fmt.Errorf("read credentials: %w", err)
	}
	var creds credentialsFile
	if err := json.Unmarshal(b, &creds); err != nil {
		return credentialsFile{}, fmt.Errorf("parse credentials %s: %w", path, err)
	}
	if creds.Sessions == nil {
		creds.Sessions = map[string]credentialSession{}
	}
	if creds.PendingDeviceLogins == nil {
		creds.PendingDeviceLogins = map[string]pendingDeviceLogin{}
	}
	return creds, nil
}

func emptyCredentials() credentialsFile {
	return credentialsFile{Sessions: map[string]credentialSession{}, PendingDeviceLogins: map[string]pendingDeviceLogin{}}
}

func canonicalizeCredentials(creds *credentialsFile) (bool, error) {
	sessions := make(map[string]credentialSession, len(creds.Sessions))
	changed := false
	for stored, session := range creds.Sessions {
		canonical, err := canonicalAuthOrigin(stored)
		if err != nil {
			return false, fmt.Errorf("saved credential origin %q is invalid: %w", stored, err)
		}
		if existing, ok := sessions[canonical]; ok && existing != session {
			return false, fmt.Errorf("saved credential origins conflict after canonicalization at %s; remove one alias explicitly", canonical)
		}
		sessions[canonical] = session
		changed = changed || canonical != stored
	}
	pending := make(map[string]pendingDeviceLogin, len(creds.PendingDeviceLogins))
	for stored, login := range creds.PendingDeviceLogins {
		canonical, err := canonicalAuthOrigin(stored)
		if err != nil {
			return false, fmt.Errorf("saved pending-login origin %q is invalid: %w", stored, err)
		}
		if existing, ok := pending[canonical]; ok && existing != login {
			return false, fmt.Errorf("saved pending-login origins conflict after canonicalization at %s; remove one alias explicitly", canonical)
		}
		pending[canonical] = login
		changed = changed || canonical != stored
	}
	creds.Sessions = sessions
	creds.PendingDeviceLogins = pending
	return changed, nil
}

func writeCredentialsUnlocked(path string, creds credentialsFile) error {
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceCredentialFile(tmpPath, path); err != nil {
		return err
	}
	return syncCredentialDirectory(filepath.Dir(path))
}
