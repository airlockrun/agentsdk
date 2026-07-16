package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/sourcebundle"
)

func TestCmdCloneCreatesBoundWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const agentID = "11111111-1111-1111-1111-111111111111"
	source := t.TempDir()
	mustWrite(t, filepath.Join(source, "go.mod"), "module cloned\n")
	mustWrite(t, filepath.Join(source, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(source, "setup.sh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(source, "setup.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	state, err := sourcebundle.Digest(source)
	if err != nil {
		t.Fatal(err)
	}
	srv := sourceServer(t, agentID, "cloned-agent", source, state)
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", "token", ""); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if err := cmdClone([]string{agentID, dst, "--url", srv.URL}); err != nil {
		t.Fatalf("cmdClone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "main.go")); err != nil {
		t.Fatalf("cloned source missing: %v", err)
	}
	binding, ok, err := loadAgentBinding(dst)
	if err != nil || !ok {
		t.Fatalf("binding = %#v, ok=%v, err=%v", binding, ok, err)
	}
	remote, ok := binding.remote(defaultRemoteName)
	if !ok || remote.AgentID != agentID || remote.SourceState != state || remote.AirlockURL != srv.URL {
		t.Fatalf("remote = %#v, ok=%v", remote, ok)
	}
	if _, err := os.Stat(filepath.Join(dst, ".airlock", "local", "agent.toml")); err != nil {
		t.Fatalf("local binding missing: %v", err)
	}
}

func TestCmdCloneRemovesCreatedDestinationOnInvalidState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const agentID = "11111111-1111-1111-1111-111111111111"
	source := t.TempDir()
	mustWrite(t, filepath.Join(source, "go.mod"), "module cloned\n")
	srv := sourceServer(t, agentID, "cloned-agent", source, "sha256:wrong")
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", "token", ""); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	err := cmdClone([]string{agentID, dst, "--url", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "response declared sha256:wrong") {
		t.Fatalf("cmdClone error = %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("failed clone destination remains: %v", err)
	}
}

func TestCmdPullRefusesTwoSidedChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const agentID = "11111111-1111-1111-1111-111111111111"
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module initial\n")
	baseState, err := sourcebundle.Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "local.txt"), "local change")
	remoteSource := t.TempDir()
	mustWrite(t, filepath.Join(remoteSource, "go.mod"), "module remote\n")
	remoteState, err := sourcebundle.Digest(remoteSource)
	if err != nil {
		t.Fatal(err)
	}
	srv := sourceServer(t, agentID, "agent", remoteSource, remoteState)
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "dev@example.com", "token", ""); err != nil {
		t.Fatal(err)
	}
	binding := agentBinding{}
	binding.putRemote(defaultRemoteName, agentRemoteBinding{
		AirlockURL: srv.URL, AgentID: agentID, Slug: "agent", SourceState: baseState,
	})
	if err := writeAgentBinding(dir, binding); err != nil {
		t.Fatal(err)
	}
	err = cmdPull([]string{dir})
	if err == nil || !strings.Contains(err.Error(), "local and Airlock source both changed") || !strings.Contains(err.Error(), "air clone") {
		t.Fatalf("cmdPull error = %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "local.txt")); err != nil || string(body) != "local change" {
		t.Fatalf("local source changed: %q, %v", body, err)
	}
}

func TestResolveSourceAirlockCloneDoesNotRebindCurrentWorkspace(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, agentBindingPath), "not valid TOML")

	flags := sourceFlags{url: "https://other.example", remote: "prod"}
	baseURL, remoteName, err := resolveSourceAirlock(dir, flags, false)
	if err != nil {
		t.Fatalf("clone resolveSourceAirlock: %v", err)
	}
	if baseURL != "https://other.example" || remoteName != "prod" {
		t.Fatalf("baseURL=%q remoteName=%q", baseURL, remoteName)
	}
	if _, _, err := resolveSourceAirlock(dir, flags, true); err == nil {
		t.Fatalf("pull resolveSourceAirlock error = %v", err)
	}
}

func TestUploadSourceSendsPrecondition(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	state, err := sourcebundle.Digest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var gotMatch, gotForce, gotMessage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMatch = r.Header.Get("If-Match")
		gotForce = r.Header.Get("X-Airlock-Force")
		gotMessage = r.Header.Get("X-Airlock-Commit-Message")
		w.Header().Set("ETag", quoteETag(state))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	got, err := uploadSource(context.Background(), srv.URL, "token", "agent", dir, state, "Add reminders", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != state || unquoteETag(gotMatch) != state || gotForce != "true" || gotMessage != "Add reminders" {
		t.Fatalf("state=%q If-Match=%q force=%q message=%q", got, gotMatch, gotForce, gotMessage)
	}
}

func sourceServer(t *testing.T, agentID, slug, source, state string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + agentID:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agent":{"id":"` + agentID + `","slug":"` + slug + `"}}`))
		case "/api/v1/agents/" + agentID + "/source":
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("ETag", quoteETag(state))
			if _, err := sourcebundle.WriteArchive(w, source); err != nil {
				t.Errorf("WriteArchive: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
}
