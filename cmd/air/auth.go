package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
	reauthenticate := false
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
		case "reauthenticate":
			reauthenticate = true
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
	}
	if check && (forceWait || noWait || noBrowser || reauthenticate) {
		return errors.New("--check cannot be combined with --wait, --no-wait, --no-browser, or --reauthenticate")
	}
	if forceWait && noWait {
		return errors.New("--wait and --no-wait cannot be combined")
	}
	if len(positional) != 1 {
		return errors.New("login requires exactly one argument: the Airlock URL")
	}
	baseURL, err := canonicalAuthOrigin(positional[0])
	if err != nil {
		return err
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
	if !reauthenticate {
		loggedIn, err := validateExistingLogin(ctx, baseURL)
		if err != nil {
			return err
		}
		if loggedIn {
			return nil
		}
	}
	return loginWithDeviceCode(ctx, baseURL, openBrowser, wait)
}

func validateExistingLogin(ctx context.Context, baseURL string) (bool, error) {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return false, err
	}
	creds, err := loadCredentials()
	if err != nil {
		return false, err
	}
	sess, ok := creds.Sessions[baseURL]
	if !ok {
		return false, nil
	}
	if sess.AccessToken == "" && sess.RefreshToken == "" {
		return false, fmt.Errorf("saved login for %s has no access or refresh token", baseURL)
	}
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		var expired *loginExpiredError
		if errors.As(err, &expired) {
			return false, nil
		}
		return false, err
	}
	var me airlockv1.MeResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/me", token, nil, &me); err != nil {
		var expired *loginExpiredError
		if errors.As(err, &expired) {
			return false, nil
		}
		if isTerminalSessionError(err) {
			if clearErr := clearLoginSessionIfMatch(baseURL, rejectedAccessToken(err, token)); clearErr != nil {
				return false, clearErr
			}
			return false, nil
		}
		return false, fmt.Errorf("validate login for %s: %w", baseURL, err)
	}
	if me.User == nil || strings.TrimSpace(me.User.GetEmail()) == "" {
		return false, fmt.Errorf("validate login for %s: server response missing user email", baseURL)
	}
	fmt.Printf("Already logged in to %s as %s. Use --reauthenticate to log in again.\n", baseURL, me.User.GetEmail())
	return true, nil
}

func loginWithDeviceCode(ctx context.Context, baseURL string, openBrowser, wait bool) error {
	var begin airlockv1.DeviceLoginBeginResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/device/begin", "", &airlockv1.DeviceLoginBeginRequest{ClientName: "air CLI", DeviceName: defaultDeviceName()}, &begin); err != nil {
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
		fmt.Printf("After approving, run: go tool air login %s --check\n", baseURL)
		return nil
	}
	fmt.Println("Waiting for approval...")
	interval := time.Duration(pending.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	for {
		if time.Now().After(pending.ExpiresAt) {
			if err := clearPendingDeviceLoginIfMatch(baseURL, pending.DeviceCode); err != nil {
				return err
			}
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
		done, err := handleDeviceLoginPoll(baseURL, pending, poll)
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
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	pending, ok := creds.PendingDeviceLogins[baseURL]
	if !ok || pending.DeviceCode == "" {
		return fmt.Errorf("no pending device login for %s; run go tool air login %s --no-wait", baseURL, baseURL)
	}
	if time.Now().After(pending.ExpiresAt) {
		if err := clearPendingDeviceLoginIfMatch(baseURL, pending.DeviceCode); err != nil {
			return err
		}
		return errors.New("device login expired")
	}
	poll, err := pollDeviceLogin(ctx, baseURL, pending)
	if err != nil {
		return err
	}
	done, err := handleDeviceLoginPoll(baseURL, pending, poll)
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

func handleDeviceLoginPoll(baseURL string, pending pendingDeviceLogin, poll *airlockv1.DeviceLoginPollResponse) (bool, error) {
	if poll == nil {
		return true, errors.New("device login returned empty response")
	}
	switch poll.Status {
	case "pending", "slow_down":
		if poll.PollIntervalSeconds > 0 {
			pending.PollIntervalSeconds = poll.PollIntervalSeconds
			if err := updatePendingDeviceLoginIfMatch(baseURL, pending); err != nil {
				return true, err
			}
		}
		return false, nil
	case "denied":
		if err := clearPendingDeviceLoginIfMatch(baseURL, pending.DeviceCode); err != nil {
			return true, errors.Join(errors.New("device login denied"), err)
		}
		return true, errors.New("device login denied")
	case "expired":
		if err := clearPendingDeviceLoginIfMatch(baseURL, pending.DeviceCode); err != nil {
			return true, errors.Join(errors.New("device login expired"), err)
		}
		return true, errors.New("device login expired")
	case "approved":
		if poll.AccessToken == "" || poll.RefreshToken == "" || poll.User == nil {
			return true, errors.New("device login approved without credentials")
		}
		if err := saveLoginCredentialsForPending(baseURL, pending.DeviceCode, poll.User.GetEmail(), poll.AccessToken, poll.RefreshToken); err != nil {
			return true, err
		}
		fmt.Printf("Logged in to %s as %s\n", baseURL, poll.User.GetEmail())
		return true, nil
	default:
		return true, fmt.Errorf("device login returned unknown status %q", poll.Status)
	}
}

func cmdLogout(args []string) error {
	if len(args) != 1 {
		return errors.New("logout requires exactly one argument: the Airlock URL")
	}
	baseURL, err := canonicalAuthOrigin(args[0])
	if err != nil {
		return err
	}
	var hadSession bool
	ctx, cancel := context.WithTimeout(context.Background(), authMutationTimeout)
	defer cancel()
	err = withCredentialsLock(ctx, func(path string) error {
		creds, err := readCredentialsUnlocked(path)
		if err != nil {
			return err
		}
		if _, err := canonicalizeCredentials(&creds); err != nil {
			return err
		}
		sess, ok := creds.Sessions[baseURL]
		hadSession = ok
		if ok && sess.RefreshToken != "" {
			revokeCtx, revokeCancel := context.WithTimeout(ctx, authRequestTimeout)
			err := doProto(revokeCtx, baseURL, httpMethodPost, "/auth/logout", "", &airlockv1.LogoutRequest{RefreshToken: sess.RefreshToken}, nil)
			revokeCancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Server session was not revoked; local login will still be removed: %v\n", err)
			}
		}
		delete(creds.Sessions, baseURL)
		delete(creds.PendingDeviceLogins, baseURL)
		return writeCredentialsUnlocked(path, creds)
	})
	if err != nil {
		return err
	}
	if hadSession {
		fmt.Printf("Logged out of %s\n", baseURL)
	} else {
		fmt.Printf("No saved login for %s\n", baseURL)
	}
	return nil
}

func defaultDeviceName() string {
	userName := strings.TrimSpace(os.Getenv("USER"))
	if userName == "" {
		userName = strings.TrimSpace(os.Getenv("USERNAME"))
	}
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	switch {
	case userName != "" && host != "":
		return userName + "@" + host
	case host != "":
		return host
	case userName != "":
		return userName
	default:
		return "air CLI"
	}
}

func deviceLoginExpiresAt(expiresInSeconds int32) time.Time {
	if expiresInSeconds <= 0 {
		expiresInSeconds = int32((10 * time.Minute) / time.Second)
	}
	return time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
}

func savePendingDeviceLogin(baseURL string, pending pendingDeviceLogin) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		creds.PendingDeviceLogins[baseURL] = pending
		return nil
	})
}

func clearPendingDeviceLoginIfMatch(baseURL, deviceCode string) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		if pending, ok := creds.PendingDeviceLogins[baseURL]; ok && pending.DeviceCode == deviceCode {
			delete(creds.PendingDeviceLogins, baseURL)
		}
		return nil
	})
}

func updatePendingDeviceLoginIfMatch(baseURL string, pending pendingDeviceLogin) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		current, ok := creds.PendingDeviceLogins[baseURL]
		if !ok || current.DeviceCode != pending.DeviceCode {
			return fmt.Errorf("pending device login for %s changed while approval was being checked", baseURL)
		}
		current.PollIntervalSeconds = pending.PollIntervalSeconds
		creds.PendingDeviceLogins[baseURL] = current
		return nil
	})
}

func clearLoginSessionIfMatch(baseURL, accessToken string) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		if sess, ok := creds.Sessions[baseURL]; ok && sess.AccessToken == accessToken {
			delete(creds.Sessions, baseURL)
		}
		return nil
	})
}

func saveLoginCredentials(baseURL, email, accessToken, refreshToken string) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		creds.Sessions[baseURL] = credentialSession{Email: email, AccessToken: accessToken, RefreshToken: refreshToken}
		delete(creds.PendingDeviceLogins, baseURL)
		return nil
	})
}

func saveLoginCredentialsForPending(baseURL, deviceCode, email, accessToken, refreshToken string) error {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return err
	}
	return mutateCredentials(context.Background(), func(creds *credentialsFile) error {
		pending, ok := creds.PendingDeviceLogins[baseURL]
		if !ok || pending.DeviceCode != deviceCode {
			return fmt.Errorf("pending device login for %s changed while approval was being checked; run go tool air login %s --check", baseURL, baseURL)
		}
		creds.Sessions[baseURL] = credentialSession{Email: email, AccessToken: accessToken, RefreshToken: refreshToken}
		delete(creds.PendingDeviceLogins, baseURL)
		return nil
	})
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

const (
	accessTokenSkew     = 60 * time.Second
	authRequestTimeout  = 20 * time.Second
	authMutationTimeout = credentialsLockTimeout + authRequestTimeout
)

type loginExpiredError struct {
	baseURL string
}

var errTokenNotManaged = errors.New("access token is not managed by the credential store")
var errRefreshUnavailable = errors.New("saved login has no refresh token")

func (e *loginExpiredError) Error() string {
	return fmt.Sprintf("login expired for %s; run go tool air login %s", e.baseURL, e.baseURL)
}

func accessTokenForURL(ctx context.Context, baseURL string) (string, error) {
	return accessTokenForURLAfterRejection(ctx, baseURL, "")
}

func accessTokenForURLAfterRejection(ctx context.Context, baseURL, rejectedAccessToken string) (token string, retErr error) {
	baseURL, err := canonicalAuthOrigin(baseURL)
	if err != nil {
		return "", err
	}
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		injectedURL := os.Getenv("AIRLOCK_API_URL")
		injectedToken := os.Getenv("AIRLOCK_TOKEN")
		if injectedURL == "" || injectedToken == "" {
			return "", errors.New("developer session requires AIRLOCK_API_URL and AIRLOCK_TOKEN")
		}
		injectedOrigin, err := canonicalAuthOrigin(injectedURL)
		if err != nil {
			return "", fmt.Errorf("invalid AIRLOCK_API_URL: %w", err)
		}
		if baseURL != injectedOrigin {
			return "", fmt.Errorf("developer session token is restricted to %s", injectedOrigin)
		}
		if rejectedAccessToken != "" {
			return "", errTokenNotManaged
		}
		return injectedToken, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, authMutationTimeout)
	defer cancel()
	err = withCredentialsLock(opCtx, func(path string) error {
		creds, err := readCredentialsUnlocked(path)
		if err != nil {
			return err
		}
		migrated, err := canonicalizeCredentials(&creds)
		if err != nil {
			return err
		}
		if migrated {
			if err := writeCredentialsUnlocked(path, creds); err != nil {
				return err
			}
		}
		sess, ok := creds.Sessions[baseURL]
		if !ok || (sess.AccessToken == "" && sess.RefreshToken == "") {
			if rejectedAccessToken != "" {
				return errTokenNotManaged
			}
			return fmt.Errorf("not logged in to %s; run go tool air login %s", baseURL, baseURL)
		}
		if rejectedAccessToken != "" && sess.AccessToken != rejectedAccessToken {
			token = sess.AccessToken
			return nil
		}
		fresh, reason := locallyFreshAccessToken(sess.AccessToken, time.Now())
		if rejectedAccessToken == "" && fresh {
			token = sess.AccessToken
			return nil
		}
		if sess.RefreshToken == "" {
			if rejectedAccessToken != "" {
				return errRefreshUnavailable
			}
			return fmt.Errorf("saved login for %s cannot refresh its %s access token; run go tool air login %s --reauthenticate", baseURL, reason, baseURL)
		}
		requestCtx, requestCancel := context.WithTimeout(opCtx, authRequestTimeout)
		var resp airlockv1.RefreshResponse
		err = doProto(requestCtx, baseURL, httpMethodPost, "/auth/refresh", "", &airlockv1.RefreshRequest{RefreshToken: sess.RefreshToken}, &resp)
		requestCancel()
		if err != nil {
			if isTerminalSessionError(err) {
				delete(creds.Sessions, baseURL)
				if saveErr := writeCredentialsUnlocked(path, creds); saveErr != nil {
					return saveErr
				}
				return &loginExpiredError{baseURL: baseURL}
			}
			return fmt.Errorf("refresh login for %s: %w; saved credentials were retained", baseURL, err)
		}
		if resp.AccessToken == "" {
			return fmt.Errorf("refresh login for %s: server response missing access_token; saved credentials were retained", baseURL)
		}
		if resp.RefreshToken == "" {
			return fmt.Errorf("refresh login for %s: server response missing refresh_token; saved credentials were retained", baseURL)
		}
		sess.AccessToken = resp.AccessToken
		sess.RefreshToken = resp.RefreshToken
		creds.Sessions[baseURL] = sess
		if err := writeCredentialsUnlocked(path, creds); err != nil {
			return err
		}
		token = sess.AccessToken
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func locallyFreshAccessToken(token string, now time.Time) (bool, string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false, "malformed"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, "malformed"
	}
	var claims struct {
		ExpiresAt  json.Number `json:"exp"`
		ClientKind string      `json:"client_kind"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil || claims.ExpiresAt == "" {
		return false, "malformed"
	}
	expiresAt, err := strconv.ParseInt(string(claims.ExpiresAt), 10, 64)
	if err != nil {
		return false, "malformed"
	}
	if claims.ClientKind != "cli" {
		return false, "missing or invalid CLI credential marker"
	}
	if time.Unix(expiresAt, 0).After(now.Add(accessTokenSkew)) {
		return true, "fresh"
	}
	return false, "expired or near-expiry"
}
