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
)

type credentialsFile struct {
	Sessions map[string]credentialSession `json:"sessions"`
}

type credentialSession struct {
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

func cmdLogin(args []string) error {
	noBrowser := false
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
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
	}
	if len(positional) != 1 {
		return errors.New("login requires exactly one argument: the Airlock URL")
	}
	baseURL := normalizeBaseURL(positional[0])
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return errors.New("Airlock URL must start with http:// or https://")
	}
	return loginWithDeviceCode(context.Background(), baseURL, !noBrowser)
}

func loginWithDeviceCode(ctx context.Context, baseURL string, openBrowser bool) error {
	var begin airlockv1.DeviceLoginBeginResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/device/begin", "", &airlockv1.DeviceLoginBeginRequest{ClientName: "air CLI"}, &begin); err != nil {
		return fmt.Errorf("begin device login: %w", err)
	}
	if begin.DeviceCode == "" || begin.UserCode == "" || begin.VerificationUrl == "" {
		return errors.New("begin device login: server response missing device_code, user_code, or verification_url")
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
	fmt.Println("Waiting for approval...")
	interval := time.Duration(begin.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(time.Duration(begin.ExpiresInSeconds) * time.Second)
	if begin.ExpiresInSeconds <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		if time.Now().After(deadline) {
			return errors.New("device login expired")
		}
		time.Sleep(interval)
		var poll airlockv1.DeviceLoginPollResponse
		if err := doProto(ctx, baseURL, httpMethodPost, "/auth/device/poll", "", &airlockv1.DeviceLoginPollRequest{DeviceCode: begin.DeviceCode}, &poll); err != nil {
			return fmt.Errorf("poll device login: %w", err)
		}
		if poll.PollIntervalSeconds > 0 {
			interval = time.Duration(poll.PollIntervalSeconds) * time.Second
		}
		switch poll.Status {
		case "pending", "slow_down":
			continue
		case "denied":
			return errors.New("device login denied")
		case "expired":
			return errors.New("device login expired")
		case "approved":
			if poll.AccessToken == "" || poll.RefreshToken == "" || poll.User == nil {
				return errors.New("device login approved without credentials")
			}
			if err := saveLoginCredentials(baseURL, poll.User.GetEmail(), poll.AccessToken, poll.RefreshToken); err != nil {
				return err
			}
			fmt.Printf("Logged in to %s as %s\n", baseURL, poll.User.GetEmail())
			return nil
		default:
			return fmt.Errorf("device login returned unknown status %q", poll.Status)
		}
	}
}

func saveLoginCredentials(baseURL, email, accessToken, refreshToken string) error {
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	creds.Sessions[baseURL] = credentialSession{Email: email, AccessToken: accessToken, RefreshToken: refreshToken}
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
			return credentialsFile{Sessions: map[string]credentialSession{}}, nil
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
