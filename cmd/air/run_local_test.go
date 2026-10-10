package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/localconfig"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestLocalRunConfigIsExplicit(t *testing.T) {
	good := localRunConfig{DatabaseEnv: "LOCAL_APP_DATABASE_URL", User: wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}, ExecutorImage: "fixture:test", Models: map[string]string{"@default": "work/openai/gpt-4o-mini"}}
	for _, tc := range []struct {
		name   string
		mutate func(*localRunConfig)
		valid  bool
	}{
		{"valid", func(*localRunConfig) {}, true},
		{"managed database", func(c *localRunConfig) { c.DatabaseEnv = "" }, true},
		{"conflicting databases", func(c *localRunConfig) { c.Postgres = &localPostgresConfig{DataDir: "/fixture"} }, false},
		{"caller", func(c *localRunConfig) { c.User.ID = "not-a-uuid" }, false},
		{"membership", func(c *localRunConfig) { c.User.PlatformMember = false }, false},
		{"model", func(c *localRunConfig) { c.Models = nil }, false},
		{"short model", func(c *localRunConfig) { c.Models["@default"] = "openai/gpt-4o-mini" }, false},
		{"alias", func(c *localRunConfig) { c.Models["@default"] = "work/fireworks/deployment" }, false},
		{"executor", func(c *localRunConfig) { c.ExecutorImage = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(good)
			var config localRunConfig
			json.Unmarshal(raw, &config)
			tc.mutate(&config)
			raw, _ = json.Marshal(config)
			_, err := decodeLocalConfig(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	raw, _ := json.Marshal(good)
	raw = append(raw, []byte(` {}`)...)
	if _, err := decodeLocalConfig(raw); err == nil {
		t.Fatal("trailing config accepted")
	}
	if _, err := decodeLocalConfig([]byte(`{"secret":"do not accept keys in project config"}`)); err == nil {
		t.Fatal("unknown config accepted")
	}
}

func TestLocalCLIStartsActualAppWithFixtureDependencies(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	database, err := postgres.Run(ctx, "pgvector/pgvector:pg17", postgres.WithDatabase("local_fixture"), postgres.WithUsername("fixture"), postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := database.Terminate(cleanup); err != nil {
			t.Error(err)
		}
	})
	dsn, err := database.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	sdkRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	t.Setenv("LOCAL_APP_DATABASE_URL", dsn)
	t.Setenv("OPENAI_API_KEY", "fixture-not-a-real-key")
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fixture app must not request a model")
		http.Error(w, "fixture only", 500)
	}))
	t.Cleanup(endpoint.Close)
	seedLocalSolConfig(t, endpoint.URL)
	module := "module example.com/local-cli-fixture\n\ngo 1.26.9\n\nrequire github.com/airlockrun/agentsdk v0.8.1-alpha.7\nreplace github.com/airlockrun/agentsdk => " + sdkRoot + "\n"
	if err := os.WriteFile("go.mod", []byte(module), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package main
import("io";"net/http";"strings";"github.com/airlockrun/agentsdk")
func main(){a:=agentsdk.New(agentsdk.Config{Description:"CLI fixture"});a.RegisterDirectory("tmp",agentsdk.DirectoryOpts{Read:agentsdk.AccessAdmin,Write:agentsdk.AccessAdmin,List:agentsdk.AccessAdmin,Description:"Fixture storage"});a.RegisterRoute(&agentsdk.Route{Method:"GET",Path:"/",Access:agentsdk.AccessAdmin,Description:"Fixture HTTP route",Handler:func(w http.ResponseWriter,r *http.Request)error{var value string;if err:=a.DB().QueryRowContext(r.Context(),"SELECT 'database ready'").Scan(&value);err!=nil{return err};if _,err:=a.WriteFile(r.Context(),"tmp/fixture.txt",strings.NewReader(value),"text/plain");err!=nil{return err};f,err:=a.OpenFile(r.Context(),"tmp/fixture.txt");if err!=nil{return err};defer f.Close();_,err=io.Copy(w,f);return err}});a.Serve()}`
	if err := os.WriteFile("main.go", []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("db/migrations", 0700); err != nil {
		t.Fatal(err)
	}
	tidy := exec.CommandContext(ctx, "go", "mod", "tidy")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("fixture tidy: %v %s", err, out)
	}
	config := localRunConfig{DatabaseEnv: "LOCAL_APP_DATABASE_URL", User: wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}, ExecutorImage: jsexec.PublishedExecutorImage, Models: map[string]string{"@default": "work/openai/gpt-4o-mini"}}
	raw, _ := json.Marshal(config)
	if err := os.WriteFile("runtime.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- runLocal(runCtx, []string{"--local", "--config", "runtime.json", "--addr", addr})
	}()
	defer func() {
		stop()
		select {
		case <-joined:
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			default:
			}
		case <-time.After(15 * time.Second):
			t.Error("local CLI did not shut down")
		}
	}()
	client := &http.Client{Timeout: time.Second}
	for {
		response, err := client.Get("http://" + addr + "/")
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				if string(body) != "database ready" {
					t.Fatalf("fixture app body=%q", body)
				}
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("local runner exited: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func seedLocalSolConfig(t *testing.T, baseURL string) {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", root)
	}
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("APPDATA", root)
	path, err := localconfig.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := localconfig.NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(t.Context(), func(c *localconfig.Config) error {
		c.Providers["work/openai"] = localconfig.ProviderConfig{Auth: "api-key", Key: "fixture-not-a-real-key", BaseURL: baseURL}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalIdentityAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	first, err := localAppID(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := localAppID(dir)
	if err != nil || first != second {
		t.Fatalf("identity %q %q %v", first, second, err)
	}
	env := localAppEnv([]string{"PATH=/usr/bin", "AIRLOCK_TOKEN=remote-session", "AIRLOCK_AGENT_TOKEN=deployed-app-secret", "AIRLOCK_API_URL=https://real.example", "AGENT_MIGRATE_DOWN_TO=0"}, map[string]string{"AIRLOCK_AGENT_TOKEN": "local-secret", "AIRLOCK_API_URL": "http://fixture"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "remote-session") || strings.Contains(joined, "deployed-app-secret") || strings.Contains(joined, "real.example") || strings.Contains(joined, "DOWN_TO") {
		t.Fatalf("inherited host credentials: %s", joined)
	}
	if !strings.Contains(joined, "local-secret") || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatal("explicit local environment missing")
	}
}

func TestLocalResourceAdapterUsesOnlyFixtureGateway(t *testing.T) {
	user := wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}
	paths := []string{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-session" {
			t.Errorf("gateway auth=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get(wire.CallerHeader) != "" || r.Header.Get("X-Airlock-Run-ID") != "" {
			t.Error("local assertions reached remote gateway")
		}
		paths = append(paths, r.URL.Path)
		var request airlockv1.InvokeConnectionRequest
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := protojson.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			return
		}
		if request.Path != "/fixture" {
			t.Errorf("upstream path=%q", request.Path)
		}
		raw, err = protojson.Marshal(&airlockv1.InvokeConnectionResponse{StatusCode: 200, Body: []byte(`{"fixture":true}`)})
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer fixture.Close()
	adapter := &localRemoteResources{target: integrationTarget{baseURL: fixture.URL, agentID: uuid.NewString()}, developerID: user.ID, token: func(context.Context) (string, error) { return "fixture-session", nil }}
	human := wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}
	app := wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: "background"}}
	for _, caller := range []wire.Caller{human, app} {
		response, err := adapter.Connection(t.Context(), caller, "bound", wire.ProxyRequest{Method: "GET", Path: "/fixture"})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(raw) != `{"fixture":true}` {
			t.Fatalf("body=%s", raw)
		}
	}
	if len(paths) != 2 || strings.Contains(paths[0], "/application/") || !strings.Contains(paths[1], "/application/") {
		t.Fatalf("gateway paths=%v", paths)
	}
	other := user
	other.ID = uuid.NewString()
	human.User = &other
	human.Initiator = &other
	if _, err := adapter.Connection(t.Context(), human, "bound", wire.ProxyRequest{Path: "/fixture"}); err == nil {
		t.Fatal("different developer accepted")
	}
	if len(paths) != 2 {
		t.Fatal("rejected local identity reached gateway")
	}
}
