package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

const testAccessToken = "e30.eyJleHAiOjQxMDI0NDQ4MDAsImNsaWVudF9raW5kIjoiY2xpIn0.signature"

func TestCmdLoginValidAccessToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var beginCalls, meCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me":
			meCalls++
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+testAccessToken {
				t.Errorf("Authorization = %q", got)
			}
			writeTestJSON(w, `{"user":{"email":"dev@example.com"}}`)
		case "/auth/device/begin":
			beginCalls++
			http.Error(w, "unexpected begin", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "stale@example.com", testAccessToken, ""); err != nil {
		t.Fatal(err)
	}
	pending := pendingDeviceLogin{
		DeviceCode:          "existing-device",
		UserCode:            "WXYZ-1234",
		VerificationURL:     srv.URL + "/device-login",
		ExpiresAt:           time.Now().Add(time.Minute),
		PollIntervalSeconds: 4,
	}
	if err := savePendingDeviceLogin(srv.URL, pending); err != nil {
		t.Fatal(err)
	}
	inputURL := "  " + srv.URL + "///  "
	output := captureCommandStdout(t, func() error {
		return cmdLogin([]string{inputURL, "--no-wait"})
	})
	want := "Already logged in to " + srv.URL + " as dev@example.com. Use --reauthenticate to log in again.\n"
	if output != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
	if meCalls != 1 || beginCalls != 0 {
		t.Fatalf("me calls = %d, begin calls = %d", meCalls, beginCalls)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	got := creds.PendingDeviceLogins[srv.URL]
	if got.DeviceCode != pending.DeviceCode || got.UserCode != pending.UserCode || got.VerificationURL != pending.VerificationURL || !got.ExpiresAt.Equal(pending.ExpiresAt) || got.PollIntervalSeconds != pending.PollIntervalSeconds {
		t.Fatalf("pending login = %#v, want %#v", got, pending)
	}
}

func TestCmdLoginRefreshesBeforeValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/auth/refresh":
			writeTestJSON(w, `{"accessToken":"fresh","refreshToken":"rotated"}`)
		case "/api/v1/me":
			if got := r.Header.Get("Authorization"); got != "Bearer fresh" {
				t.Errorf("Authorization = %q", got)
			}
			writeTestJSON(w, `{"user":{"email":"refreshed@example.com"}}`)
		case "/auth/device/begin":
			t.Fatal("device login began for a valid refreshed session")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "old", "refresh"); err != nil {
		t.Fatal(err)
	}
	output := captureCommandStdout(t, func() error { return cmdLogin([]string{srv.URL}) })
	if !strings.Contains(output, "as refreshed@example.com") {
		t.Fatalf("output = %q", output)
	}
	if got := strings.Join(paths, ","); got != "/auth/refresh,/api/v1/me" {
		t.Fatalf("paths = %q", got)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Sessions[srv.URL].AccessToken; got != "fresh" {
		t.Fatalf("saved access token = %q", got)
	}
	if got := creds.Sessions[srv.URL].RefreshToken; got != "rotated" {
		t.Fatalf("saved refresh token = %q", got)
	}
}

func TestCmdLoginRejectedAuthStartsDeviceLogin(t *testing.T) {
	tests := []struct {
		name         string
		refreshToken string
		rejectedPath string
		status       int
		detail       string
		wantBegin    bool
	}{
		{name: "typed refresh rejection", refreshToken: "refresh", rejectedPath: "/auth/refresh", status: http.StatusUnauthorized, detail: "invalid_refresh_token", wantBegin: true},
		{name: "untyped forbidden", rejectedPath: "/api/v1/me", status: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var beginCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tt.rejectedPath:
					writeTestJSONStatus(w, tt.status, `{"error":"rejected","detail":"`+tt.detail+`"}`)
				case "/auth/device/begin":
					beginCalls++
					writeDeviceBegin(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			access := testAccessToken
			if tt.refreshToken != "" {
				access = "expired"
			}
			if err := saveLoginCredentials(srv.URL, "dev@example.com", access, tt.refreshToken); err != nil {
				t.Fatal(err)
			}
			err := cmdLogin([]string{srv.URL, "--no-wait", "--no-browser"})
			if tt.wantBegin && err != nil {
				t.Fatalf("cmdLogin: %v", err)
			}
			if !tt.wantBegin && err == nil {
				t.Fatal("cmdLogin returned nil for an untyped rejection")
			}
			wantBeginCalls := 0
			if tt.wantBegin {
				wantBeginCalls = 1
			}
			if beginCalls != wantBeginCalls {
				t.Fatalf("begin calls = %d", beginCalls)
			}
			creds, err := loadCredentials()
			if err != nil {
				t.Fatal(err)
			}
			_, sessionRemains := creds.Sessions[srv.URL]
			if sessionRemains == tt.wantBegin {
				t.Fatalf("session presence = %v, want %v", sessionRemains, !tt.wantBegin)
			}
			if tt.wantBegin && creds.PendingDeviceLogins[srv.URL].DeviceCode != "device-secret" {
				t.Fatalf("pending login = %#v", creds.PendingDeviceLogins[srv.URL])
			}
		})
	}
}

func TestCmdLoginFailureDoesNotBeginDeviceLogin(t *testing.T) {
	tests := []struct {
		name         string
		refreshToken string
		failurePath  string
		body         string
		status       int
	}{
		{name: "refresh server error", refreshToken: "refresh", failurePath: "/auth/refresh", body: `{"error":"unavailable"}`, status: http.StatusServiceUnavailable},
		{name: "me server error", failurePath: "/api/v1/me", body: `{"error":"unavailable"}`, status: http.StatusServiceUnavailable},
		{name: "malformed refresh success", refreshToken: "refresh", failurePath: "/auth/refresh", body: `{}`, status: http.StatusOK},
		{name: "malformed me success", failurePath: "/api/v1/me", body: `{}`, status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var beginCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tt.failurePath:
					writeTestJSONStatus(w, tt.status, tt.body)
				case "/auth/device/begin":
					beginCalls++
					writeDeviceBegin(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			access := testAccessToken
			if tt.refreshToken != "" {
				access = "expired"
			}
			if err := saveLoginCredentials(srv.URL, "dev@example.com", access, tt.refreshToken); err != nil {
				t.Fatal(err)
			}
			err := cmdLogin([]string{srv.URL, "--no-wait"})
			if err == nil {
				t.Fatal("cmdLogin returned nil error")
			}
			if beginCalls != 0 {
				t.Fatalf("begin calls = %d", beginCalls)
			}
			creds, loadErr := loadCredentials()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(creds.PendingDeviceLogins) != 0 {
				t.Fatalf("pending logins modified: %#v", creds.PendingDeviceLogins)
			}
		})
	}
}

func TestCmdLoginReauthenticateBypassesValidationAndPreservesSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var beginCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device/begin":
			beginCalls++
			writeDeviceBegin(w, r)
		case "/auth/refresh", "/api/v1/me":
			t.Fatalf("existing session was validated at %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	want := credentialSession{Email: "dev@example.com", AccessToken: "access", RefreshToken: "refresh"}
	if err := saveLoginCredentials(srv.URL, want.Email, want.AccessToken, want.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := cmdLogin([]string{srv.URL, "--reauthenticate", "--no-wait", "--no-browser"}); err != nil {
		t.Fatalf("cmdLogin: %v", err)
	}
	if beginCalls != 1 {
		t.Fatalf("begin calls = %d", beginCalls)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Sessions[srv.URL]; got != want {
		t.Fatalf("session = %#v, want %#v", got, want)
	}
	if creds.PendingDeviceLogins[srv.URL].DeviceCode == "" {
		t.Fatal("pending device login was not saved")
	}
}

func TestCmdLoginFlagConflicts(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "check and reauthenticate", args: []string{"https://airlock.example.com", "--check", "--reauthenticate"}, want: "--check cannot be combined"},
		{name: "wait and no-wait", args: []string{"https://airlock.example.com", "--wait", "--no-wait"}, want: "--wait and --no-wait cannot be combined"},
		{name: "no alias", args: []string{"https://airlock.example.com", "--reauth"}, want: "unknown flag --reauth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cmdLogin(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("cmdLogin error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestCmdLoginCheckPollsBeforeSessionValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var pollCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device/poll":
			pollCalls++
			writeTestJSON(w, `{"status":"pending"}`)
		case "/api/v1/me", "/auth/refresh", "/auth/device/begin":
			t.Fatalf("unexpected request to %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", ""); err != nil {
		t.Fatal(err)
	}
	if err := savePendingDeviceLogin(srv.URL, pendingDeviceLogin{
		DeviceCode:      "pending-device",
		ExpiresAt:       time.Now().Add(time.Minute),
		VerificationURL: srv.URL + "/device-login",
		UserCode:        "ABCD-EFGH",
	}); err != nil {
		t.Fatal(err)
	}
	err := cmdLogin([]string{srv.URL, "--check"})
	if err == nil || err.Error() != "device login still pending" {
		t.Fatalf("cmdLogin error = %v", err)
	}
	if pollCalls != 1 {
		t.Fatalf("poll calls = %d", pollCalls)
	}
}

func TestDeviceLoginPendingState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	baseURL := "https://airlock.example.com/"
	pending := pendingDeviceLogin{
		DeviceCode:          "device-secret",
		UserCode:            "ABCD-EFGH",
		VerificationURL:     "https://airlock.example.com/device-login",
		ExpiresAt:           time.Now().Add(10 * time.Minute),
		PollIntervalSeconds: 3,
	}
	if err := savePendingDeviceLogin(baseURL, pending); err != nil {
		t.Fatalf("savePendingDeviceLogin: %v", err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if got := creds.PendingDeviceLogins["https://airlock.example.com"]; got.DeviceCode != pending.DeviceCode || got.UserCode != pending.UserCode {
		t.Fatalf("pending = %#v", got)
	}
	done, err := handleDeviceLoginPoll("https://airlock.example.com", pending, &airlockv1.DeviceLoginPollResponse{
		Status:       "approved",
		AccessToken:  "access",
		RefreshToken: "refresh",
		User:         &airlockv1.User{Email: "dev@example.com"},
	})
	if err != nil || !done {
		t.Fatalf("handleDeviceLoginPoll done=%v err=%v", done, err)
	}
	creds, err = loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials after approve: %v", err)
	}
	if _, ok := creds.PendingDeviceLogins["https://airlock.example.com"]; ok {
		t.Fatalf("pending was not cleared: %#v", creds.PendingDeviceLogins)
	}
	if got := creds.Sessions["https://airlock.example.com"]; got.Email != "dev@example.com" || got.AccessToken != "access" || got.RefreshToken != "refresh" {
		t.Fatalf("session = %#v", got)
	}
}

func TestDeviceLoginApprovalDoesNotOverwriteNewerPendingFlow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	baseURL := "https://airlock.example.com"
	oldPending := pendingDeviceLogin{DeviceCode: "old", ExpiresAt: time.Now().Add(time.Minute)}
	newPending := pendingDeviceLogin{DeviceCode: "new", ExpiresAt: time.Now().Add(time.Minute)}
	if err := savePendingDeviceLogin(baseURL, oldPending); err != nil {
		t.Fatal(err)
	}
	if err := savePendingDeviceLogin(baseURL, newPending); err != nil {
		t.Fatal(err)
	}
	_, err := handleDeviceLoginPoll(baseURL, oldPending, &airlockv1.DeviceLoginPollResponse{
		Status:       "approved",
		AccessToken:  testAccessToken,
		RefreshToken: "old-refresh",
		User:         &airlockv1.User{Email: "old@example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("handleDeviceLoginPoll error = %v", err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Sessions[baseURL]; ok {
		t.Fatal("stale approval created a session")
	}
	if got := creds.PendingDeviceLogins[baseURL].DeviceCode; got != "new" {
		t.Fatalf("pending device code = %q, want new", got)
	}
}

func TestCmdLogoutRevokesAndClearsSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var sawLogout bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/logout" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		sawLogout = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", "refresh"); err != nil {
		t.Fatalf("saveLoginCredentials: %v", err)
	}
	if err := cmdLogout([]string{srv.URL}); err != nil {
		t.Fatalf("cmdLogout: %v", err)
	}
	if !sawLogout {
		t.Fatal("logout endpoint was not called")
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if _, ok := creds.Sessions[normalizeBaseURL(srv.URL)]; ok {
		t.Fatalf("session was not cleared: %#v", creds.Sessions)
	}
}

func TestCmdLogoutUsesLatestRotatedCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	logoutToken := make(chan string, 1)
	fresh := testJWT(time.Now().Add(10 * time.Minute))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			close(refreshStarted)
			<-releaseRefresh
			writeTestJSON(w, fmt.Sprintf(`{"accessToken":%q,"refreshToken":"rotated"}`, fresh))
		case "/auth/logout":
			var request airlockv1.LogoutRequest
			body, _ := io.ReadAll(r.Body)
			if err := protoUnmarshal.Unmarshal(body, &request); err != nil {
				t.Errorf("decode logout: %v", err)
			}
			logoutToken <- request.RefreshToken
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", testJWT(time.Now().Add(30*time.Second)), "initial"); err != nil {
		t.Fatal(err)
	}
	refreshDone := make(chan error, 1)
	go func() {
		_, err := accessTokenForURL(context.Background(), srv.URL)
		refreshDone <- err
	}()
	<-refreshStarted
	logoutDone := make(chan error, 1)
	go func() { logoutDone <- cmdLogout([]string{srv.URL}) }()
	close(releaseRefresh)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-logoutDone; err != nil {
		t.Fatal(err)
	}
	if got := <-logoutToken; got != "rotated" {
		t.Fatalf("logout refresh token = %q, want rotated", got)
	}
}

func TestCmdLogoutClearsLocalLoginWhenServerRevokeFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSONStatus(w, http.StatusServiceUnavailable, `{"error":"unavailable"}`)
	}))
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", testAccessToken, "refresh"); err != nil {
		t.Fatal(err)
	}
	stderr := captureCommandStderr(t, func() error { return cmdLogout([]string{srv.URL}) })
	if !strings.Contains(stderr, "Server session was not revoked; local login will still be removed") {
		t.Fatalf("stderr = %q", stderr)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Sessions[srv.URL]; ok {
		t.Fatal("local session remains after explicit logout")
	}
}

func TestAccessTokenClearsExpiredLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		writeTestJSONStatus(w, http.StatusUnauthorized, `{"error":"invalid refresh token","detail":"invalid_refresh_token"}`)
	}))
	defer srv.Close()

	if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", "refresh"); err != nil {
		t.Fatalf("saveLoginCredentials: %v", err)
	}
	_, err := accessTokenForURL(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("accessTokenForURL returned nil error")
	}
	if want := "login expired for " + normalizeBaseURL(srv.URL) + "; run go tool air login " + normalizeBaseURL(srv.URL); err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials: %v", err)
	}
	if _, ok := creds.Sessions[normalizeBaseURL(srv.URL)]; ok {
		t.Fatalf("expired session was not cleared: %#v", creds.Sessions)
	}
}

func TestAccessTokenRefreshIsSerializedAcrossProcesses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{}, 1)
	refreshed := testJWT(time.Now().Add(10 * time.Minute))
	if fresh, reason := locallyFreshAccessToken(refreshed, time.Now()); !fresh {
		t.Fatalf("test JWT is not fresh: %s", reason)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
		var request airlockv1.RefreshRequest
		body, _ := io.ReadAll(r.Body)
		if err := protoUnmarshal.Unmarshal(body, &request); err != nil || request.RefreshToken != "initial-refresh" {
			t.Errorf("refresh request token = %q, err = %v", request.RefreshToken, err)
		}
		time.Sleep(150 * time.Millisecond)
		writeTestJSON(w, fmt.Sprintf(`{"accessToken":%q,"refreshToken":"rotated"}`, refreshed))
	}))
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", testJWT(time.Now().Add(30*time.Second)), "initial-refresh"); err != nil {
		t.Fatal(err)
	}
	commands := []*exec.Cmd{
		authHelperCommand("access", srv.URL),
		authHelperCommand("access", srv.URL),
	}
	errs := make(chan error, len(commands))
	for _, command := range commands {
		go func(command *exec.Cmd) { errs <- command.Run() }(command)
	}
	<-refreshStarted
	if err := authHelperCommand("save", "https://other.example.com").Run(); err != nil {
		t.Fatalf("concurrent profile save: %v", err)
	}
	for range commands {
		if err := <-errs; err != nil {
			t.Fatalf("auth subprocess: %v", err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Sessions[srv.URL]; got.AccessToken != refreshed || got.RefreshToken != "rotated" {
		t.Fatalf("refreshed session = %#v", got)
	}
	if got := creds.Sessions["https://other.example.com"]; got.Email != "other@example.com" || got.RefreshToken != "other-refresh" {
		t.Fatalf("unrelated session = %#v", got)
	}
}

func TestCredentialLockIsReleasedWhenProcessExits(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := authHelperCommand("exit-with-lock", "").Run(); err != nil {
		t.Fatalf("lock subprocess: %v", err)
	}
	if err := saveLoginCredentials("https://airlock.example.com", "dev@example.com", testAccessToken, "refresh"); err != nil {
		t.Fatalf("save after subprocess exit: %v", err)
	}
}

func TestCredentialLockWaitHonorsContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withCredentialsLock(context.Background(), func(string) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := withCredentialsLock(ctx, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for credential lock") {
		t.Fatalf("withCredentialsLock error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAuthProcessHelper(t *testing.T) {
	if os.Getenv("AIR_AUTH_HELPER") != "1" {
		return
	}
	switch os.Getenv("AIR_AUTH_HELPER_OPERATION") {
	case "access":
		if _, err := accessTokenForURL(context.Background(), os.Getenv("AIR_AUTH_HELPER_ORIGIN")); err != nil {
			os.Exit(2)
		}
	case "exit-with-lock":
		if err := withCredentialsLock(context.Background(), func(string) error { os.Exit(0); return nil }); err != nil {
			os.Exit(3)
		}
	case "save":
		if err := saveLoginCredentials(os.Getenv("AIR_AUTH_HELPER_ORIGIN"), "other@example.com", testAccessToken, "other-refresh"); err != nil {
			os.Exit(5)
		}
	default:
		os.Exit(4)
	}
}

func TestRefreshErrorCredentialRetention(t *testing.T) {
	for _, code := range []string{"session_expired", "session_revoked", "session_auth_epoch_changed", "invalid_refresh_token", ""} {
		name := code
		if name == "" {
			name = "untyped"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeTestJSONStatus(w, http.StatusUnauthorized, `{"error":"refresh rejected","detail":"`+code+`"}`)
			}))
			defer srv.Close()
			if err := saveLoginCredentials(srv.URL, "dev@example.com", "malformed", "refresh"); err != nil {
				t.Fatal(err)
			}
			if err := saveLoginCredentials("https://other.example.com", "other@example.com", testAccessToken, "other"); err != nil {
				t.Fatal(err)
			}
			_, accessErr := accessTokenForURL(context.Background(), srv.URL)
			if accessErr == nil {
				t.Fatal("accessTokenForURL returned nil")
			}
			creds, err := loadCredentials()
			if err != nil {
				t.Fatal(err)
			}
			_, remains := creds.Sessions[srv.URL]
			if remains != (code == "") {
				t.Fatalf("target session remains = %v", remains)
			}
			if _, ok := creds.Sessions["https://other.example.com"]; !ok {
				t.Fatal("terminal refresh removed an unrelated profile")
			}
		})
	}
}

func TestMalformedAccessTokensAreRefreshed(t *testing.T) {
	for _, token := range []string{"opaque", "e30.e30.signature", "e30.eyJleHAiOiJub3QtYS10aW1lIn0.signature"} {
		t.Run(token, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var calls int
			fresh := testJWT(time.Now().Add(10 * time.Minute))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				writeTestJSON(w, fmt.Sprintf(`{"accessToken":%q,"refreshToken":"rotated"}`, fresh))
			}))
			defer srv.Close()
			if err := saveLoginCredentials(srv.URL, "dev@example.com", token, "refresh"); err != nil {
				t.Fatal(err)
			}
			got, err := accessTokenForURL(context.Background(), srv.URL)
			if err != nil || got != fresh || calls != 1 {
				t.Fatalf("token = %q, calls = %d, err = %v", got, calls, err)
			}
		})
	}
}

func TestAuthenticatedRequestRetriesOneUnauthorized(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	initial := testJWT(time.Now().Add(10 * time.Minute))
	refreshed := testJWT(time.Now().Add(20 * time.Minute))
	var resourceCalls, refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			refreshCalls++
			writeTestJSON(w, fmt.Sprintf(`{"accessToken":%q,"refreshToken":"rotated"}`, refreshed))
		case "/api/v1/me":
			resourceCalls++
			if r.Header.Get("Authorization") == "Bearer "+initial {
				writeTestJSONStatus(w, http.StatusUnauthorized, `{"error":"access token rejected"}`)
				return
			}
			writeTestJSON(w, `{"user":{"email":"dev@example.com"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", initial, "refresh"); err != nil {
		t.Fatal(err)
	}
	var me airlockv1.MeResponse
	if err := doProto(context.Background(), srv.URL, http.MethodGet, "/api/v1/me", initial, nil, &me); err != nil {
		t.Fatal(err)
	}
	if resourceCalls != 2 || refreshCalls != 1 {
		t.Fatalf("resource calls = %d, refresh calls = %d", resourceCalls, refreshCalls)
	}
}

func TestCanonicalAuthOrigin(t *testing.T) {
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: " HTTPS://Example.COM:443/// ", want: "https://example.com", ok: true},
		{raw: "http://Example.COM:80/", want: "http://example.com", ok: true},
		{raw: "https://example.com:8443", want: "https://example.com:8443", ok: true},
		{raw: "https://example.com/path"},
		{raw: "https://example.com?x=1"},
		{raw: "https://example.com#fragment"},
		{raw: "https://user@example.com"},
		{raw: "ftp://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := canonicalAuthOrigin(tt.raw)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("canonicalAuthOrigin() = %q, %v", got, err)
			}
		})
	}
}

func TestCredentialAliasMigrationAndModes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"sessions":{"HTTPS://Example.COM:443///":{"email":"dev@example.com","accessToken":"` + testAccessToken + `"}},"pendingDeviceLogins":{}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := creds.Sessions["https://example.com"]; !ok || len(creds.Sessions) != 1 {
		t.Fatalf("sessions = %#v", creds.Sessions)
	}
	for name, target := range map[string]string{"directory": filepath.Dir(path), "file": path} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if name == "file" {
			want = 0o600
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", name, got, want)
		}
	}
}

func TestCredentialAliasConflictIsExplicit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"sessions":{"https://example.com":{"email":"one@example.com"},"HTTPS://EXAMPLE.COM:443/":{"email":"two@example.com"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("loadCredentials error = %v", err)
	}
}

func TestAuthenticatedRedirectDoesNotLeakToken(t *testing.T) {
	var targetAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuthorization = r.Header.Get("Authorization")
		writeTestJSON(w, `{}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/me", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	var me airlockv1.MeResponse
	err := doProto(context.Background(), source.URL, http.MethodGet, "/api/v1/me", testAccessToken, nil, &me)
	if err == nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("doProto error = %v", err)
	}
	if targetAuthorization != "" {
		t.Fatalf("redirect target received Authorization %q", targetAuthorization)
	}
}

func authHelperCommand(operation, origin string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuthProcessHelper$")
	cmd.Env = append(os.Environ(), "AIR_AUTH_HELPER=1", "AIR_AUTH_HELPER_OPERATION="+operation, "AIR_AUTH_HELPER_ORIGIN="+origin)
	return cmd
}

func testJWT(expiry time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"client_kind":"cli"}`, expiry.Unix())))
	return "e30." + payload + ".signature"
}

func captureCommandStderr(t *testing.T, run func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	runErr := run()
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("command error: %v", runErr)
	}
	return string(body)
}

func writeDeviceBegin(w http.ResponseWriter, r *http.Request) {
	verificationURL := "http://" + r.Host + "/device-login"
	writeTestJSON(w, `{"deviceCode":"device-secret","userCode":"ABCD-EFGH","verificationUrl":"`+verificationURL+`","expiresInSeconds":600,"pollIntervalSeconds":3}`)
}

func writeTestJSON(w http.ResponseWriter, body string) {
	writeTestJSONStatus(w, http.StatusOK, body)
}

func writeTestJSONStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
