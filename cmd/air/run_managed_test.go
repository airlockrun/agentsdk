package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
)

type cliFixture struct {
	command  *exec.Cmd
	url      string
	done     chan error
	logsDone chan struct{}
}

func startManagedCLIFixture(t *testing.T, ctx context.Context, binary, dir string) *cliFixture {
	t.Helper()
	command := exec.CommandContext(ctx, binary, "run", "--local", "--config", "runtime.json")
	command.Dir = dir
	command.Env = localAppEnv(os.Environ(), map[string]string{"GOWORK": "off", "OPENAI_API_KEY": "fixture-only-key"})
	command.Stderr = io.Discard
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &cliFixture{command: command, done: make(chan error, 1), logsDone: make(chan struct{})}
	ready := make(chan string, 1)
	go func() {
		defer close(fixture.logsDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "Local app ready at ") {
				ready <- strings.Fields(scanner.Text())[4]
			}
		}
	}()
	go func() { fixture.done <- command.Wait() }()
	t.Cleanup(func() { fixture.stop(t) })
	select {
	case fixture.url = <-ready:
		return fixture
	case err := <-fixture.done:
		t.Fatalf("managed CLI exited during startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil
}
func (f *cliFixture) stop(t *testing.T) {
	t.Helper()
	if f.command == nil {
		return
	}
	_ = f.command.Process.Signal(os.Interrupt)
	select {
	case err := <-f.done:
		if err != nil {
			t.Errorf("managed CLI shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		f.command.Process.Kill()
		t.Error("managed CLI did not shut down")
	}
	<-f.logsDone
	f.command = nil
}
func fixtureHTTP(t *testing.T, base, method, path string) string {
	t.Helper()
	request, _ := http.NewRequest(method, base+path, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("fixture HTTP %d %s", response.StatusCode, data)
	}
	return string(data)
}

func TestManagedCLITwoAppsAndRestart(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	if os.Getuid() <= 0 {
		t.Skip("bind-mount PostgreSQL requires a non-root invoking user")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	sdkRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	binary := filepath.Join(root, "air")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/air")
	build.Dir = sdkRoot
	// Build the CLI against the workspace's shared Sol API.
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CLI fixture build: %v %s", err, out)
	}
	fakeModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fixture app requested a model")
		http.Error(w, "fixture only", 500)
	}))
	defer fakeModel.Close()
	seedLocalSolConfig(t, fakeModel.URL)
	var dirs []string
	for index := range 2 {
		dir := filepath.Join(root, fmt.Sprintf("app%d", index))
		if err := os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0700); err != nil {
			t.Fatal(err)
		}
		module := fmt.Sprintf("module example.com/managed%d\n\ngo 1.26.9\nrequire github.com/airlockrun/agentsdk v0.8.1-alpha.8\nreplace github.com/airlockrun/agentsdk => %q\n", index, sdkRoot)
		source := `package main
import("fmt";"net/http";"github.com/airlockrun/agentsdk")
func main(){a:=agentsdk.New(agentsdk.Config{Description:"Managed CLI fixture"});a.RegisterRoute(&agentsdk.Route{Method:"GET",Path:"/",Access:agentsdk.AccessPublic,Description:"Count retained rows",Handler:func(w http.ResponseWriter,r *http.Request)error{var count int;if err:=a.DB().QueryRowContext(r.Context(),"SELECT count(*) FROM retained").Scan(&count);err!=nil{return err};fmt.Fprint(w,count);return nil}});a.RegisterRoute(&agentsdk.Route{Method:"POST",Path:"/insert",Access:agentsdk.AccessAdmin,Description:"Insert fixture row",Handler:func(w http.ResponseWriter,r *http.Request)error{_,err:=a.DB().ExecContext(r.Context(),"INSERT INTO retained VALUES ('fixture')");return err}});a.Serve()}`
		for name, data := range map[string]string{"go.mod": module, "main.go": source, "db/migrations/00001_retained.sql": "-- +goose Up\nCREATE TABLE retained (value text NOT NULL);\n-- +goose Down\nDROP TABLE retained;\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		config := localRunConfig{User: wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}, ExecutorImage: jsexec.PublishedExecutorImage, Models: map[string]string{"@default": "work/openai/gpt-4o-mini"}}
		data, _ := json.Marshal(config)
		os.WriteFile(filepath.Join(dir, "runtime.json"), data, 0600)
		tidy := exec.CommandContext(ctx, "go", "mod", "tidy")
		tidy.Dir = dir
		tidy.Env = append(os.Environ(), "GOWORK=off")
		if out, err := tidy.CombinedOutput(); err != nil {
			t.Fatalf("fixture tidy: %v %s", err, out)
		}
		dirs = append(dirs, dir)
	}
	first := startManagedCLIFixture(t, ctx, binary, dirs[0])
	second := startManagedCLIFixture(t, ctx, binary, dirs[1])
	if first.url == second.url {
		t.Fatal("default app ports collide")
	}
	if fixtureHTTP(t, first.url, "GET", "/") != "0" || fixtureHTTP(t, second.url, "GET", "/") != "0" {
		t.Fatal("new fixture database not empty")
	}
	fixtureHTTP(t, first.url, "POST", "/insert")
	if fixtureHTTP(t, first.url, "GET", "/") != "1" || fixtureHTTP(t, second.url, "GET", "/") != "0" {
		t.Fatal("app database isolation failed")
	}
	first.stop(t)
	restarted := startManagedCLIFixture(t, ctx, binary, dirs[0])
	if fixtureHTTP(t, restarted.url, "GET", "/") != "1" {
		t.Fatal("managed CLI restart lost database data")
	}
	restarted.stop(t)
	second.stop(t)
}

func TestLocalConfigurationRejectsUnsetExternalDSN(t *testing.T) {
	t.Setenv("EMPTY_LOCAL_DATABASE", "")
	dir := t.TempDir()
	t.Chdir(dir)
	config := localRunConfig{DatabaseEnv: "EMPTY_LOCAL_DATABASE", User: wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}, ExecutorImage: "fixture:test", Models: map[string]string{"@default": "work/openai/gpt-4o-mini"}}
	raw, _ := json.Marshal(config)
	os.WriteFile("runtime.json", raw, 0600)
	err := runLocal(t.Context(), []string{"--local", "--config", "runtime.json"})
	if err == nil || !strings.Contains(err.Error(), "EMPTY_LOCAL_DATABASE") {
		t.Fatalf("unset external DSN fell back: %v", err)
	}
	if _, err := os.Stat(".airlock"); !os.IsNotExist(err) {
		t.Fatal("missing external DSN created managed state")
	}
}

func TestLocalDataPathCannotEnterSourceBundle(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := localPostgresDataDir("db/local-data"); err == nil {
		t.Fatal("exportable source directory accepted")
	}
	if _, err := localPostgresDataDir(".airlock/local/postgres"); err != nil {
		t.Fatal(err)
	}
	if _, err := localPostgresDataDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitLocalPortIsNotReassigned(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if other, err := localDeveloperListener(listener.Addr().String()); err == nil {
		other.Close()
		t.Fatal("occupied local address accepted")
	} else if !strings.Contains(err.Error(), "choose --addr") {
		t.Fatalf("missing port guidance: %v", err)
	}
}
