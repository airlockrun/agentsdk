package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"golang.org/x/term"
)

type credentialsFile struct {
	Sessions            map[string]credentialSession  `json:"sessions"`
	PendingDeviceLogins map[string]pendingDeviceLogin `json:"pendingDeviceLogins,omitempty"`
}

type credentialSession struct {
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

type pendingDeviceLogin struct {
	DeviceCode          string    `json:"deviceCode"`
	UserCode            string    `json:"userCode"`
	VerificationURL     string    `json:"verificationUrl"`
	ExpiresAt           time.Time `json:"expiresAt"`
	PollIntervalSeconds int32     `json:"pollIntervalSeconds,omitempty"`
}

func cmdLogin(args []string) error {
	noBrowser := false
	forceWait := false
	noWait := false
	check := false
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[:2] != "--" {
			positional = append(positional, a)
			continue
		}
		switch key := a[2:]; key {
		case "no-browser":
			noBrowser = true
		case "wait":
			forceWait = true
		case "no-wait":
			noWait = true
		case "check":
			check = true
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
	}
	if check && (forceWait || noWait || noBrowser) {
		return errors.New("--check cannot be combined with --wait, --no-wait, or --no-browser")
	}
	if forceWait && noWait {
		return errors.New("--wait and --no-wait cannot be combined")
	}
	if len(positional) != 1 {
		return errors.New("login requires exactly one argument: the Airlock URL")
	}
	baseURL := normalizeBaseURL(positional[0])
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return errors.New("Airlock URL must start with http:// or https://")
	}
	ctx := context.Background()
	if check {
		return checkDeviceLogin(ctx, baseURL)
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	wait := interactive
	if forceWait {
		wait = true
	}
	if noWait {
		wait = false
	}
	openBrowser := interactive && !noBrowser
	return loginWithDeviceCode(ctx, baseURL, openBrowser, wait)
}

func loginWithDeviceCode(ctx context.Context, baseURL string, openBrowser, wait bool) error {
	var begin airlockv1.DeviceLoginBeginResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/device/begin", "", &airlockv1.DeviceLoginBeginRequest{ClientName: "air CLI"}, &begin); err != nil {
		return fmt.Errorf("begin device login: %w", err)
	}
	if begin.DeviceCode == "" || begin.UserCode == "" || begin.VerificationUrl == "" {
		return errors.New("begin device login: server response missing device_code, user_code, or verification_url")
	}
	pending := pendingDeviceLogin{
		DeviceCode:          begin.DeviceCode,
		UserCode:            begin.UserCode,
		VerificationURL:     begin.VerificationUrl,
		ExpiresAt:           deviceLoginExpiresAt(begin.ExpiresInSeconds),
		PollIntervalSeconds: begin.PollIntervalSeconds,
	}
	if err := savePendingDeviceLogin(baseURL, pending); err != nil {
		return err
	}
	fmt.Println("Open this URL to log in:")
	fmt.Printf("  %s\n\n", begin.VerificationUrl)
	fmt.Println("Enter this code in the browser:")
	fmt.Printf("  %s\n\n", begin.UserCode)
	if openBrowser {
		if err := openURL(begin.VerificationUrl); err != nil {
			fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		}
	}
	if !wait {
		fmt.Printf("After approving, run: air login %s --check\n", baseURL)
		return nil
	}
	fmt.Println("Waiting for approval...")
	interval := time.Duration(pending.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	for {
		if time.Now().After(pending.ExpiresAt) {
			_ = clearPendingDeviceLogin(baseURL)
			return errors.New("device login expired")
		}
		time.Sleep(interval)
		poll, err := pollDeviceLogin(ctx, baseURL, pending)
		if err != nil {
			return err
		}
		if poll.PollIntervalSeconds > 0 {
			interval = time.Duration(poll.PollIntervalSeconds) * time.Second
		}
		done, err := handleDeviceLoginPoll(baseURL, poll)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		return nil
	}
}

func checkDeviceLogin(ctx context.Context, baseURL string) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	pending, ok := creds.PendingDeviceLogins[normalizeBaseURL(baseURL)]
	if !ok || pending.DeviceCode == "" {
		return fmt.Errorf("no pending device login for %s; run air login %s --no-wait", baseURL, baseURL)
	}
	if time.Now().After(pending.ExpiresAt) {
		delete(creds.PendingDeviceLogins, normalizeBaseURL(baseURL))
		if err := saveCredentials(creds); err != nil {
			return err
		}
		return errors.New("device login expired")
	}
	poll, err := pollDeviceLogin(ctx, baseURL, pending)
	if err != nil {
		return err
	}
	done, err := handleDeviceLoginPoll(baseURL, poll)
	if err != nil {
		return err
	}
	if !done {
		if poll.Status == "slow_down" && poll.PollIntervalSeconds > 0 {
			return fmt.Errorf("device login still pending; wait at least %d seconds before checking again", poll.PollIntervalSeconds)
		}
		return errors.New("device login still pending")
	}
	return nil
}

func pollDeviceLogin(ctx context.Context, baseURL string, pending pendingDeviceLogin) (*airlockv1.DeviceLoginPollResponse, error) {
	var poll airlockv1.DeviceLoginPollResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/device/poll", "", &airlockv1.DeviceLoginPollRequest{DeviceCode: pending.DeviceCode}, &poll); err != nil {
		return nil, fmt.Errorf("poll device login: %w", err)
	}
	return &poll, nil
}

func handleDeviceLoginPoll(baseURL string, poll *airlockv1.DeviceLoginPollResponse) (bool, error) {
	if poll == nil {
		return true, errors.New("device login returned empty response")
	}
	switch poll.Status {
	case "pending", "slow_down":
		return false, nil
	case "denied":
		_ = clearPendingDeviceLogin(baseURL)
		return true, errors.New("device login denied")
	case "expired":
		_ = clearPendingDeviceLogin(baseURL)
		return true, errors.New("device login expired")
	case "approved":
		if poll.AccessToken == "" || poll.RefreshToken == "" || poll.User == nil {
			return true, errors.New("device login approved without credentials")
		}
		if err := saveLoginCredentials(baseURL, poll.User.GetEmail(), poll.AccessToken, poll.RefreshToken); err != nil {
			return true, err
		}
		fmt.Printf("Logged in to %s as %s\n", baseURL, poll.User.GetEmail())
		return true, nil
	default:
		return true, fmt.Errorf("device login returned unknown status %q", poll.Status)
	}
}

func deviceLoginExpiresAt(expiresInSeconds int32) time.Time {
	if expiresInSeconds <= 0 {
		expiresInSeconds = int32((10 * time.Minute) / time.Second)
	}
	return time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
}

func savePendingDeviceLogin(baseURL string, pending pendingDeviceLogin) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	creds.PendingDeviceLogins[normalizeBaseURL(baseURL)] = pending
	return saveCredentials(creds)
}

func clearPendingDeviceLogin(baseURL string) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	delete(creds.PendingDeviceLogins, normalizeBaseURL(baseURL))
	return saveCredentials(creds)
}

func saveLoginCredentials(baseURL, email, accessToken, refreshToken string) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	baseURL = normalizeBaseURL(baseURL)
	creds.Sessions[baseURL] = credentialSession{Email: email, AccessToken: accessToken, RefreshToken: refreshToken}
	delete(creds.PendingDeviceLogins, baseURL)
	return saveCredentials(creds)
}

func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

const httpMethodPost = "POST"

func accessTokenForURL(ctx context.Context, baseURL string) (string, error) {
	creds, err := loadCredentials()
	if err != nil {
		return "", err
	}
	sess, ok := creds.Sessions[normalizeBaseURL(baseURL)]
	if !ok || (sess.AccessToken == "" && sess.RefreshToken == "") {
		return "", fmt.Errorf("not logged in to %s; run air login %s", baseURL, baseURL)
	}
	if sess.RefreshToken == "" {
		return sess.AccessToken, nil
	}
	var resp airlockv1.RefreshResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/refresh", "", &airlockv1.RefreshRequest{RefreshToken: sess.RefreshToken}, &resp); err != nil {
		return "", fmt.Errorf("refresh login for %s: %w", baseURL, err)
	}
	sess.AccessToken = resp.AccessToken
	creds.Sessions[normalizeBaseURL(baseURL)] = sess
	if err := saveCredentials(creds); err != nil {
		return "", err
	}
	return sess.AccessToken, nil
}

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "airlock", "credentials.json"), nil
}

func loadCredentials() (credentialsFile, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentialsFile{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return credentialsFile{Sessions: map[string]credentialSession{}, PendingDeviceLogins: map[string]pendingDeviceLogin{}}, nil
		}
		return credentialsFile{}, err
	}
	var creds credentialsFile
	if err := json.Unmarshal(b, &creds); err != nil {
		return credentialsFile{}, err
	}
	if creds.Sessions == nil {
		creds.Sessions = map[string]credentialSession{}
	}
	if creds.PendingDeviceLogins == nil {
		creds.PendingDeviceLogins = map[string]pendingDeviceLogin{}
	}
	return creds, nil
}

func saveCredentials(creds credentialsFile) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
