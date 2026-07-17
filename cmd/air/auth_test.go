package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

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
			if got := r.Header.Get("Authorization"); got != "Bearer access" {
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

	if err := saveLoginCredentials(srv.URL, "stale@example.com", "access", ""); err != nil {
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
			writeTestJSON(w, `{"accessToken":"fresh"}`)
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
}

func TestCmdLoginRejectedAuthStartsDeviceLogin(t *testing.T) {
	tests := []struct {
		name         string
		refreshToken string
		rejectedPath string
		status       int
	}{
		{name: "refresh unauthorized", refreshToken: "refresh", rejectedPath: "/auth/refresh", status: http.StatusUnauthorized},
		{name: "me forbidden", rejectedPath: "/api/v1/me", status: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var beginCalls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tt.rejectedPath:
					http.Error(w, `{"error":"rejected"}`, tt.status)
				case "/auth/device/begin":
					beginCalls++
					writeDeviceBegin(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", tt.refreshToken); err != nil {
				t.Fatal(err)
			}
			if err := cmdLogin([]string{srv.URL, "--no-wait", "--no-browser"}); err != nil {
				t.Fatalf("cmdLogin: %v", err)
			}
			if beginCalls != 1 {
				t.Fatalf("begin calls = %d", beginCalls)
			}
			creds, err := loadCredentials()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := creds.Sessions[srv.URL]; ok {
				t.Fatalf("rejected session remains: %#v", creds.Sessions)
			}
			if creds.PendingDeviceLogins[srv.URL].DeviceCode != "device-secret" {
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

			if err := saveLoginCredentials(srv.URL, "dev@example.com", "access", tt.refreshToken); err != nil {
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
	done, err := handleDeviceLoginPoll("https://airlock.example.com", &airlockv1.DeviceLoginPollResponse{
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

func TestAccessTokenClearsExpiredLogin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		http.Error(w, `{"error":"invalid refresh token"}`, http.StatusUnauthorized)
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
