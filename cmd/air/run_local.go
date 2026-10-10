package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/airlockrun/agentsdk/buildconfig"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/localruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/localconfig"
	"github.com/airlockrun/sol/provider"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

type localRunConfig struct {
	DatabaseEnv     string               `json:"databaseEnv"`
	User            wire.CallerUser      `json:"user"`
	Models          map[string]string    `json:"models"`
	ExecutorImage   string               `json:"executorImage"`
	ResourcesRemote string               `json:"resourcesRemote,omitempty"`
	EnvVars         map[string]string    `json:"envVars,omitempty"`
	Postgres        *localPostgresConfig `json:"postgres,omitempty"`
}
type localPostgresConfig struct {
	DataDir string `json:"dataDir,omitempty"`
	Image   string `json:"image,omitempty"`
}

func decodeLocalConfig(raw []byte) (localRunConfig, error) {
	var config localRunConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return config, errors.New("local runtime config contains trailing JSON")
	}
	if strings.ContainsAny(config.DatabaseEnv, " =\t\r\n") {
		return config, errors.New("local runtime requires databaseEnv naming an explicit dedicated PostgreSQL DSN environment variable")
	}
	if config.DatabaseEnv != "" && config.Postgres != nil {
		return config, errors.New("databaseEnv and managed postgres configuration are mutually exclusive")
	}
	caller := wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &config.User, Initiator: &config.User, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}
	userID, userErr := uuid.Parse(config.User.ID)
	if userErr != nil || userID == uuid.Nil || userID.String() != config.User.ID {
		return config, errors.New("local developer user ID must be a canonical non-nil UUID")
	}
	if err := caller.Validate(); err != nil {
		return config, fmt.Errorf("local developer identity: %w", err)
	}
	if config.ExecutorImage == "" {
		return config, errors.New("local runtime requires executorImage; build the packaged recipe with --build-executor")
	}
	if _, ok := config.Models["@default"]; !ok {
		return config, errors.New("local runtime requires an explicit @default model binding")
	}
	for slot, selected := range config.Models {
		ref, err := localconfig.ParseModelRef(selected)
		if err != nil {
			return config, fmt.Errorf("model slot %q: %w", slot, err)
		}
		if err := provider.ValidateLocalEntry(ref.Entry); err != nil {
			return config, fmt.Errorf("model slot %q: %w", slot, err)
		}
	}
	return config, nil
}

func cmdRun(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runLocal(ctx, args)
}

func runLocal(parent context.Context, args []string) (runErr error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	local := fs.Bool("local", false, "run the app with the local v3 host")
	configPath := fs.String("config", ".airlock/local/runtime.json", "explicit local runtime configuration")
	addr := fs.String("addr", "127.0.0.1:0", "loopback developer HTTP listener; port 0 allocates an available port")
	buildExecutor := fs.Bool("build-executor", false, "build the packaged Deno executor image")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: go tool air run --local [--config path] [--addr 127.0.0.1:0] [--build-executor]\nPostgreSQL is auto-started unless databaseEnv is selected. See reference/local-development.md.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if !*local || len(fs.Args()) != 0 {
		return errors.New("run requires --local and accepts no positional arguments")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("local runtime --addr must use a loopback IP")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read local runtime config: %w", err)
	}
	config, err := decodeLocalConfig(raw)
	if err != nil {
		return err
	}
	dsn := ""
	if config.DatabaseEnv != "" {
		dsn = os.Getenv(config.DatabaseEnv)
		if dsn == "" {
			return fmt.Errorf("local runtime database environment %s is empty", config.DatabaseEnv)
		}
	}
	if _, err := buildconfig.Load("."); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(parent)
	defer stop()
	if *buildExecutor {
		if strings.Contains(config.ExecutorImage, "@") {
			return errors.New("--build-executor requires a local development image tag, not an immutable published digest")
		}
		if err := jsexec.BuildImage(ctx, config.ExecutorImage); err != nil {
			return err
		}
	}
	executorImage, err := jsexec.PrepareDockerImage(ctx, config.ExecutorImage)
	if err != nil {
		return err
	}
	stateDir, err := filepath.Abs(".airlock/local/runtime")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	runLock := flock.New(filepath.Join(stateDir, "run.lock"))
	locked, err := runLock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("this local app is already running")
	}
	defer runLock.Unlock()
	appID, err := localAppID(stateDir)
	if err != nil {
		return err
	}
	if config.DatabaseEnv == "" {
		dataDir := ".airlock/local/postgres"
		image := ""
		if config.Postgres != nil {
			if config.Postgres.DataDir != "" {
				dataDir = config.Postgres.DataDir
			}
			image = config.Postgres.Image
		}
		dataDir, err = localPostgresDataDir(dataDir)
		if err != nil {
			return err
		}
		pg, err := localruntime.StartManagedPostgres(ctx, localruntime.PostgresConfig{AppID: appID, Directory: dataDir, Image: image})
		if err != nil {
			return err
		}
		dsn = pg.DSN
		defer func() {
			if err := pg.Close(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("stop local PostgreSQL: %w", err))
			}
		}()
	}
	binary := filepath.Join(stateDir, "app")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, ".")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build local app: %w", err)
	}
	inspect := exec.CommandContext(ctx, binary)
	inspect.Env = localAppEnv(os.Environ(), map[string]string{"AIRLOCK_AGENT_MODE": "manifest", "AIRLOCK_AGENT_ID": "", "AIRLOCK_API_URL": "", "AIRLOCK_AGENT_TOKEN": "", "AIRLOCK_DB_URL": ""})
	inspect.Stderr = os.Stderr
	manifestRaw, err := inspect.Output()
	if err != nil {
		return fmt.Errorf("inspect app manifest: %w", err)
	}
	var manifest wire.AgentManifest
	dec := json.NewDecoder(bytes.NewReader(manifestRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return fmt.Errorf("decode app manifest: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("app manifest has trailing data")
	}
	if err := wire.CheckAppRuntimeProtocol(manifest.RuntimeProtocol); err != nil {
		return err
	}
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		return err
	}
	slots := map[string]bool{"@default": true}
	for _, slot := range manifest.ModelSlots {
		if slot.Capability != "text" && slot.Capability != "vision" {
			return fmt.Errorf("local model slot %q has unsupported capability %q; local streaming slots require text or vision", slot.Slug, slot.Capability)
		}
		slots[slot.Slug] = true
		if _, ok := config.Models[slot.Slug]; !ok {
			return fmt.Errorf("local model slot %q is not configured", slot.Slug)
		}
	}
	for slot := range config.Models {
		if !slots[slot] {
			return fmt.Errorf("local model slot %q is not declared", slot)
		}
	}
	envVars := map[string]string{}
	for slug, name := range config.EnvVars {
		declared := false
		for _, decl := range manifest.EnvVars {
			if decl.Slug == slug {
				declared = true
			}
		}
		if !declared {
			return fmt.Errorf("local environment slot %q is not declared", slug)
		}
		if name == "" || strings.ContainsAny(name, " =\t\r\n") {
			return fmt.Errorf("local environment slot %q requires an environment-variable name", slug)
		}
		value, present := os.LookupEnv(name)
		if !present {
			return fmt.Errorf("environment variable %s for local slot %q is not set", name, slug)
		}
		envVars[slug] = value
	}
	storageDir := filepath.Join(stateDir, "storage")
	if err := os.MkdirAll(storageDir, 0700); err != nil {
		return err
	}
	store, err := localruntime.NewFileStorage(storageDir)
	if err != nil {
		return err
	}
	defer store.Close()
	models := map[string]localruntime.ModelBinding{}
	for slot, selected := range config.Models {
		model, limits, err := provider.ResolveLocalModel(ctx, provider.LocalModelOptions{Model: selected, HTTPClient: http.DefaultClient, UserAgent: "agentsdk-local", SessionID: uuid.NewString()})
		if err != nil {
			return fmt.Errorf("resolve local model slot %q: %w", slot, err)
		}
		if slot == "@default" {
			slot = ""
		}
		models[slot] = localruntime.ModelBinding{Model: &localruntime.StorageModel{Model: model, Storage: store, Manifest: manifest}, Limits: limits}
	}
	executor, err := localruntime.Executor(localruntime.ExecutorConfig{Image: executorImage, Limits: jsexec.DefaultLimits()})
	if err != nil {
		return err
	}
	state, err := localruntime.OpenState(stateDir, appID)
	if err != nil {
		return err
	}
	var resources localruntime.Resources
	resourceSession := ""
	discovery := capability.Discovery{}
	if config.ResourcesRemote != "" {
		target, err := resolveIntegrationTarget(ctx, integrationTargetFlags{remote: config.ResourcesRemote})
		if err != nil {
			return err
		}
		if target.codegen {
			return errors.New("local run resources require a live developer CLI session")
		}
		var me airlockv1.MeResponse
		if err := doProto(ctx, target.baseURL, "GET", "/api/v1/me", target.token, nil, &me); err != nil {
			return err
		}
		if me.User == nil || me.User.Id != config.User.ID {
			return errors.New("configured local developer must match the authenticated resource-session user")
		}
		_, admitted, err := admitLocalResourceToken(ctx, target, target.token)
		if err != nil {
			return err
		}
		if admitted.UserID != me.User.Id || admitted.AppID != target.agentID {
			return errors.New("local resource admission identity mismatch")
		}
		resourceSession = admitted.SessionID
		adapter := &localRemoteResources{target: target, developerID: me.User.Id, localCredential: true, token: func(ctx context.Context) (string, error) {
			developerToken, err := accessTokenForURL(ctx, target.baseURL)
			if err != nil {
				return "", err
			}
			token, scope, err := admitLocalResourceToken(ctx, target, developerToken)
			if err != nil {
				return "", err
			}
			if scope != admitted {
				return "", errors.New("local resource developer session changed; restart local run explicitly")
			}
			return token, nil
		}}
		discovery, err = adapter.discovery(ctx, manifest, config.User)
		if err != nil {
			return err
		}
		resources = adapter
	}
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer control.Close()
	appPort, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	appAddr := appPort.Addr().String()
	defer appPort.Close()
	appURL := "http://" + appAddr
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	client := &http.Client{Transport: http.DefaultTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	direct := localruntime.NewDirectResources(manifest, client)
	if discovery.MCPSchemas == nil {
		discovery.MCPSchemas = map[string][]wire.MCPToolSchema{}
	}
	for _, need := range manifest.MCPServers {
		if need.AuthMode == wire.MCPAuthNone {
			schemas, err := direct.Discover(ctx, need.Slug)
			if err != nil {
				return fmt.Errorf("discover local no-auth MCP %q: %w", need.Slug, err)
			}
			discovery.MCPSchemas[need.Slug] = schemas
		}
	}
	public, err := localDeveloperListener(*addr)
	if err != nil {
		return err
	}
	defer public.Close()
	runtimeHost, err := localruntime.New(localruntime.Config{AppID: appID, Token: token, AppURL: appURL, RouteURL: "http://" + public.Addr().String(), State: state, Storage: store, Models: models, Client: client, ExecutorFactory: executor, Resources: resources, NoAuthResources: direct, ResourceSession: resourceSession, Discovery: discovery, EnvVars: envVars})
	if err != nil {
		return err
	}
	controlServer := &http.Server{Handler: runtimeHost.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go controlServer.Serve(control)
	defer controlServer.Close()
	app := exec.CommandContext(ctx, binary)
	app.Stdout = os.Stdout
	app.Stderr = os.Stderr
	migrationsDir, err := filepath.Abs("db/migrations")
	if err != nil {
		return err
	}
	app.Env = localAppEnv(os.Environ(), map[string]string{"AIRLOCK_AGENT_MODE": "", "AIRLOCK_AGENT_ID": appID, "AIRLOCK_API_URL": "http://" + control.Addr().String(), "AIRLOCK_AGENT_TOKEN": token, "AIRLOCK_DB_URL": dsn, "AIRLOCK_ADDR": appAddr, "AIRLOCK_MIGRATIONS_DIR": migrationsDir, "AGENTSDK_TEST_MIGRATIONS": "", "AGENT_VALIDATE_MIGRATIONS": "", "AGENT_MIGRATE_DOWN_TO": "", "AGENT_MIGRATE_UP_ONLY": ""})
	app.Cancel = func() error { return app.Process.Signal(os.Interrupt) }
	app.WaitDelay = 10 * time.Second
	closeActivation, err := inheritLocalListener(app, appPort)
	if err != nil {
		return err
	}
	defer closeActivation()
	if err := app.Start(); err != nil {
		return err
	}
	closeActivation()
	appPort.Close()
	appDone := make(chan error, 1)
	appJoined := make(chan struct{})
	go func() { defer close(appJoined); appDone <- app.Wait() }()
	defer func() {
		stop()
		select {
		case <-appJoined:
		case <-time.After(12 * time.Second):
			app.Process.Kill()
		}
	}()
	readyCtx, readyStop := context.WithTimeout(ctx, 2*time.Minute)
	defer readyStop()
	for {
		req, _ := http.NewRequestWithContext(readyCtx, "GET", appURL+"/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-appDone:
			return fmt.Errorf("local app exited during startup: %w", err)
		case <-readyCtx.Done():
			return errors.New("local app did not become ready; inspect startup output")
		case <-time.After(100 * time.Millisecond):
		}
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	workersDone := make(chan error, 1)
	workersJoined := make(chan struct{})
	go func() { defer close(workersJoined); workersDone <- runtimeHost.Run(workerCtx) }()
	defer func() { stopWorkers(); <-workersJoined }()
	publicServer := &http.Server{Handler: localIngress(runtimeHost, appURL, token, client, manifest, config.User), ReadHeaderTimeout: 5 * time.Second}
	defer publicServer.Close()
	publicDone := make(chan error, 1)
	go func() { publicDone <- publicServer.Serve(public) }()
	fmt.Printf("Local app ready at http://%s (app %s)\n", public.Addr(), appID)
	select {
	case <-ctx.Done():
	case err := <-appDone:
		if err != nil {
			return fmt.Errorf("local app exited: %w", err)
		}
	case err := <-workersDone:
		if err != nil {
			return err
		}
	case err := <-publicDone:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	stop()
	stopWorkers()
	shutdown, shutdownStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownStop()
	publicServer.Shutdown(shutdown)
	return nil
}

func localDeveloperListener(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind local developer address %s: %w; choose --addr 127.0.0.1:0 or a free explicit port", addr, err)
	}
	return listener, nil
}

func localPostgresDataDir(value string) (string, error) {
	root, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	// Resolve an existing parent before creating the dedicated data directory so
	// a symlink cannot turn an excluded path into an exportable source subtree.
	existing := dir
	var suffix []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		suffix = append(suffix, filepath.Base(existing))
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", errors.New("cannot resolve local PostgreSQL data directory")
		}
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	inside := relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
	if inside && relative != filepath.Join(".airlock", "local") && !strings.HasPrefix(relative, filepath.Join(".airlock", "local")+string(os.PathSeparator)) {
		return "", errors.New("PostgreSQL data inside an app must be under .airlock/local/; choose an external path otherwise")
	}
	return resolved, nil
}

func localAppID(directory string) (string, error) {
	name := filepath.Join(directory, "app-id")
	raw, err := os.ReadFile(name)
	if err == nil {
		id := strings.TrimSpace(string(raw))
		parsed, e := uuid.Parse(id)
		if e != nil || parsed == uuid.Nil || parsed.String() != id {
			return "", errors.New("invalid persisted local app ID")
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id := uuid.NewString()
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return localAppID(directory)
	}
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(id + "\n")
	return id, errors.Join(err, f.Sync(), f.Close())
}

func localAppEnv(inherited []string, values map[string]string) []string {
	out := make([]string, 0, len(inherited)+len(values))
	for _, value := range inherited {
		key, _, _ := strings.Cut(value, "=")
		if _, ok := values[key]; ok {
			continue
		}
		if strings.HasPrefix(key, "AIRLOCK_") || strings.HasPrefix(key, "AGENT_MIGRATE_") {
			continue
		}
		out = append(out, value)
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func localIngress(host *localruntime.Host, appURL, token string, client *http.Client, manifest wire.AgentManifest, user wire.CallerUser) http.Handler {
	target, _ := url.Parse(appURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.Transport
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostname, _, err := net.SplitHostPort(r.Host)
		if err != nil || net.ParseIP(hostname) == nil || !net.ParseIP(hostname).IsLoopback() {
			http.Error(w, "local loopback host required", 403)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
			http.Error(w, "local same-origin request required", 403)
			return
		}
		if r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "local origin required", 403)
			return
		}
		if r.URL.Path == wire.RuntimeInvokePath || strings.HasPrefix(r.URL.Path, "/job/") || strings.HasPrefix(r.URL.Path, "/webhook/") || r.URL.Path == "/refresh" {
			http.NotFound(w, r)
			return
		}
		caller := wire.Caller{Kind: "user", Access: wire.AccessAdmin, User: &user, Initiator: &user, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}}
		scope, err := host.Admit(r.Context(), caller)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := host.CloseInvocation(ctx, scope.RunID); err != nil {
				fmt.Fprintln(os.Stderr, "air: close local invocation:", err)
			}
		}()
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-airlock-") || key == "X-Run-Id" || key == "X-Bridge-Id" {
				r.Header.Del(key)
			}
		}
		if strings.HasPrefix(r.URL.Path, "/__air/local/tools/") {
			if r.Method != "POST" {
				http.Error(w, "POST required", 405)
				return
			}
			catalog, err := capability.Catalog(manifest, capability.Discovery{})
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				http.Error(w, "invalid tool input", 400)
				return
			}
			backend := &localruntime.AppBackend{URL: appURL, Token: token, Client: client, Scope: scope, Catalog: catalog}
			result, err := backend.Invoke(r.Context(), chatruntime.Invocation{CapabilityID: capability.Local(capability.Tool, "", strings.TrimPrefix(r.URL.Path, "/__air/local/tools/")).ID(), ToolCallID: uuid.NewString(), Input: raw})
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Run-ID", scope.RunID)
		r.Header.Set(wire.InvocationTokenHeader, scope.InvocationToken)
		encoded, err := wire.EncodeCallerHeader(caller)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		r.Header.Set(wire.CallerHeader, encoded)
		proxy.ServeHTTP(w, r)
	})
}
