// Package agenttest provides agent environments, platform mocks, and caller
// contexts for tests of agents built on agentsdk.
package agenttest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/localruntime"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const postgresImage = "pgvector/pgvector:pg17"

// Env is a started SDK app with the shared local host and isolated dependencies.
type Env struct {
	Agent *agentsdk.Agent
	// Airlock records calls made through the platform API.
	Airlock *MockAirlock
	Storage *FileStorage
	// URL is the recorded local callback host's base URL.
	URL     string
	models  map[string]modelSlot
	appID   string
	Runtime *localruntime.Host
}

// New invokes factory first while runtime environment is cleared, then
// provisions the shared local host and test database and starts the app. Start opens
// the database, validates migrations with an up, down-to-zero, up cycle from the
// enclosing Go module, synchronizes declarations, and runs named OnStart hooks
// before New returns.
// HTTP requests to Agent.Handler require Authorization: Bearer test-token,
// including requests with WithUser or WithCaller test contexts.
// Routes and webhooks also require SetCallerHeader on the test request.
// TEST_DB_URL is used when explicitly supplied; otherwise New starts a
// throwaway pgvector container. The factory may wire Agent.DB()'s late-bound
// handle, but database operations are unavailable until Start. `go tool air
// build` supplies one shared throwaway database to its serial package tests.
func New(t *testing.T, factory func() *agentsdk.Agent) *Env {
	t.Helper()
	return NewWithOptions(t, factory, Options{})
}

// NewWithOptions constructs an environment with per-slot mocks and explicit live
// selections. Models are prepared before Start, including OnStart hooks. Zero
// options use only in-process mocks. RegisterFlags opts a test package into CLI
// model selection; no flags or credentials are discovered automatically.
func NewWithOptions(t *testing.T, factory func() *agentsdk.Agent, options Options) *Env {
	t.Helper()
	if factory == nil {
		t.Fatal("agenttest: factory is required")
	}

	t.Setenv("AIRLOCK_AGENT_MODE", "")
	t.Setenv("AIRLOCK_AGENT_ID", "")
	t.Setenv("AIRLOCK_API_URL", "")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "")
	t.Setenv("AIRLOCK_DB_URL", "")
	t.Setenv("AIRLOCK_MIGRATIONS_DIR", "")
	a := factory()
	if a == nil {
		t.Fatal("agenttest: factory returned nil")
	}
	if options.Storage == nil {
		options.Storage = NewFileStorage(t)
	}

	models, err := prepareModels(t.Context(), a.Manifest(), options, resolveLocalModel)
	if err != nil {
		t.Fatal(err)
	}
	for slug, selected := range models {
		selected.model = &storageModel{Model: selected.model, storage: options.Storage, manifest: a.Manifest()}
		models[slug] = selected
	}
	mock, url := newMockWithModels(models)
	mock.mock.SetStorage(options.Storage.store)
	t.Cleanup(mock.Close)
	const appID = "00000000-0000-0000-0000-000000000000"
	state, err := localruntime.OpenState(t.TempDir(), appID)
	if err != nil {
		t.Fatal(err)
	}
	factoryExecutor := options.ExecutorFactory
	if factoryExecutor == nil {
		factoryExecutor, err = localruntime.Executor(localruntime.ExecutorConfig{Image: "agentsdk-jsexecutor:local", Limits: jsexec.DefaultLimits()})
		if err != nil {
			t.Fatal(err)
		}
	}
	bindings := map[string]localruntime.ModelBinding{}
	for slug, selected := range models {
		bindings[slug] = localruntime.ModelBinding{Model: selected.model, Limits: selected.limits}
	}
	appServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.Handler().ServeHTTP(w, r) }))
	appServer.Start()
	t.Cleanup(appServer.Close)
	runtimeHost, err := localruntime.New(localruntime.Config{AppID: appID, Token: "test-token", AppURL: appServer.URL, RouteURL: appServer.URL, State: state, Storage: options.Storage.store, Models: bindings, Client: appServer.Client(), ExecutorFactory: factoryExecutor, Resources: options.Resources, NoAuthResources: options.NoAuthResources, ResourceSession: options.ResourceSession, Discovery: options.Discovery, Platform: options.Backend, EnvVars: options.EnvVars})
	if err != nil {
		t.Fatal(err)
	}
	mock.mock.SetRuntime(runtimeHost.Handler())
	t.Setenv("AIRLOCK_API_URL", url)
	t.Setenv("AIRLOCK_AGENT_ID", appID)
	t.Setenv("AIRLOCK_AGENT_TOKEN", "test-token")
	t.Setenv("AGENT_VALIDATE_MIGRATIONS", "")
	t.Setenv("AGENT_MIGRATE_DOWN_TO", "")
	t.Setenv("AGENT_MIGRATE_UP_ONLY", "")
	t.Setenv("AGENTSDK_TEST_MIGRATIONS", "1")

	// Tests only opt into an existing database through TEST_DB_URL.
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		ctr, err := postgres.Run(ctx, postgresImage,
			postgres.WithDatabase("agent_test"),
			postgres.WithUsername("agent"),
			postgres.WithPassword("agent"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("agenttest: start PostgreSQL container: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := ctr.Terminate(ctx); err != nil {
				t.Errorf("agenttest: stop PostgreSQL container: %v", err)
			}
		})
		dsn, err = ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			t.Fatalf("agenttest: PostgreSQL connection string: %v", err)
		}
	}
	t.Setenv("AIRLOCK_DB_URL", dsn)

	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("agenttest: start agent: %v", err)
	}
	workerCtx, stopWorkers := context.WithCancel(t.Context())
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		if err := runtimeHost.Run(workerCtx); err != nil {
			t.Errorf("agenttest: local runtime workers: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("agenttest: close agent database: %v", err)
		}
	})
	t.Cleanup(func() { stopWorkers(); <-workersDone })
	return &Env{Agent: a, Airlock: mock, Storage: options.Storage, URL: url, models: models, appID: appID, Runtime: runtimeHost}
}
