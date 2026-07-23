package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

type commandCall struct {
	dir  string
	name string
	args []string
}

func TestInitBootstrapsSelectedTool(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	var calls []commandCall
	stubLauncher(t, &calls)

	if err := run([]string{"init", dir, "--url", "https://airlock.example.com/"}); err != nil {
		t.Fatal(err)
	}
	want := []commandCall{
		{dir: dir, name: "go", args: []string{"get", "-tool", "github.com/airlockrun/agentsdk/cmd/air@v0.4.0-rc.32"}},
		{dir: dir, name: "go", args: []string{"tool", "air", "init", ".", "--url", "https://airlock.example.com"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestCloneBootstrapsLogsInAndClones(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	var calls []commandCall
	stubLauncher(t, &calls)

	if err := run([]string{"clone", "todo", dir, "--remote", "prod", "--url", "https://airlock.example.com"}); err != nil {
		t.Fatal(err)
	}
	want := []commandCall{
		{dir: dir, name: "go", args: []string{"get", "-tool", "github.com/airlockrun/agentsdk/cmd/air@v0.4.0-rc.32"}},
		{dir: dir, name: "go", args: []string{"tool", "air", "login", "https://airlock.example.com", "--wait"}},
		{dir: dir, name: "go", args: []string{"tool", "air", "clone", "todo", ".", "--url", "https://airlock.example.com", "--remote", "prod"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestDelegateUsesRepositoryTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module agent\n\ntool github.com/airlockrun/agentsdk/cmd/air\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var calls []commandCall
	oldExecute := execute
	execute = func(dir, name string, args ...string) error {
		calls = append(calls, commandCall{dir: dir, name: name, args: append([]string(nil), args...)})
		return nil
	}
	t.Cleanup(func() { execute = oldExecute })

	if err := run([]string{"build"}); err != nil {
		t.Fatal(err)
	}
	want := []commandCall{{dir: dir, name: "go", args: []string{"tool", "air", "build"}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestRejectsUnsafeBootstrapDestination(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []commandCall
	stubLauncher(t, &calls)
	err := run([]string{"init", dir, "--url", "https://airlock.example.com"})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("run error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("commands ran for unsafe destination: %#v", calls)
	}
}

func TestFetchSDKInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sdkInfoPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, sdkInfoPath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.4.0-rc.32","commandImport":"github.com/airlockrun/agentsdk/cmd/air","futureField":"ignored"}`))
	}))
	defer srv.Close()

	info, err := fetchSDKInfo(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if info.GetVersion() != "0.4.0-rc.32" || info.GetCommandImport() != "github.com/airlockrun/agentsdk/cmd/air" {
		t.Fatalf("info = %#v", info)
	}
}

func stubLauncher(t *testing.T, calls *[]commandCall) {
	t.Helper()
	oldExecute, oldLoadInfo := execute, loadInfo
	loadInfo = func(context.Context, string) (*airlockv1.GetAgentSDKInfoResponse, error) {
		return &airlockv1.GetAgentSDKInfoResponse{
			Version:       "0.4.0-rc.32",
			CommandImport: "github.com/airlockrun/agentsdk/cmd/air",
		}, nil
	}
	execute = func(dir, name string, args ...string) error {
		*calls = append(*calls, commandCall{dir: dir, name: name, args: append([]string(nil), args...)})
		return nil
	}
	t.Cleanup(func() {
		execute = oldExecute
		loadInfo = oldLoadInfo
	})
}
