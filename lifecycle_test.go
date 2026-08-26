package agentsdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader consumed before runtime check") }

func TestOnStartRunsInRegistrationOrder(t *testing.T) {
	a, _ := testAgent(t)
	var got []string
	a.OnStart("first", func(context.Context) error {
		got = append(got, "first")
		return nil
	})
	a.OnStart("second", func(context.Context) error {
		got = append(got, "second")
		return nil
	})
	if err := a.runStartHooks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("startup order = %v, want %v", got, want)
	}
}

func TestOnStartFailsLoudly(t *testing.T) {
	a, _ := testAgent(t)
	want := errors.New("hydrate failed")
	a.OnStart("hydrate", func(context.Context) error { return want })
	err := a.runStartHooks(t.Context())
	if !errors.Is(err, want) || err.Error() != `agentsdk: startup hook "hydrate": hydrate failed` {
		t.Fatalf("startup error = %v", err)
	}
}

func TestOnStartRejectsInvalidRegistration(t *testing.T) {
	a, _ := testAgent(t)
	expectPanicContains(t, "name is required", func() {
		a.OnStart("", func(context.Context) error { return nil })
	})
	expectPanicContains(t, "callback is required", func() {
		a.OnStart("hydrate", nil)
	})
	a.OnStart("hydrate", func(context.Context) error { return nil })
	expectPanicContains(t, "duplicate OnStart", func() {
		a.OnStart("hydrate", func(context.Context) error { return nil })
	})
}

func TestDefinitionOnlyAgentRejectsRuntimeOperationsBeforeWork(t *testing.T) {
	a := New(Config{Description: "definition only"})
	conn := a.RegisterConnection(&Connection{
		Slug: "api", Name: "API", Description: "API", BaseURL: "https://example.com",
		AuthMode: ConnectionAuthNone, Access: AccessAdmin,
	})
	mcp := a.RegisterMCP(&MCP{
		Slug: "tools", Name: "Tools", URL: "https://example.com/mcp",
		AuthMode: MCPAuthNone, Access: AccessAdmin,
	})
	topic := a.RegisterTopic(&Topic{Slug: "alerts", Description: "Alerts", Access: AccessUser, PerUser: true})
	env := a.RegisterEnvVar(&EnvVar{Slug: "api_key", Description: "API key"})

	assertUnavailable := func(operation string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), operation+" is unavailable before the agent runtime starts") {
			t.Fatalf("%s error = %v", operation, err)
		}
	}
	ctx := context.Background()
	_, err := a.WriteFile(ctx, "cache/data", panicReader{}, "application/octet-stream")
	assertUnavailable("WriteFile", err)
	_, err = conn.RequestStream(ctx, RequestOpts{Body: panicReader{}})
	assertUnavailable("ConnectionHandle.RequestStream", err)
	_, err = mcp.CallTool(ctx, "query", panicReader{})
	assertUnavailable("MCPHandle.CallTool", err)
	assertUnavailable("TopicHandle.Publish", topic.Publish(ctx, nil))
	_, err = env.Get(ctx)
	assertUnavailable("EnvVarHandle.Get", err)
	_, err = a.Seal(ctx, "secret")
	assertUnavailable("Seal", err)

	localDir := filepath.Join(t.TempDir(), "not-created")
	assertUnavailable("SyncDown", a.SyncDown(ctx, "cache", localDir))
	if _, err := os.Stat(localDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SyncDown touched local directory before runtime: %v", err)
	}
	expectPanicContains(t, "LLM is unavailable before the agent runtime starts", func() {
		a.LLM(ctx, "undeclared")
	})
}

func TestRuntimeUnavailableUntilDependenciesAreInitialized(t *testing.T) {
	a := New(Config{Description: "initializing"})
	if err := a.beginStart(); err != nil {
		t.Fatal(err)
	}
	if a.runtimeAvailable() {
		t.Fatal("runtime is available during dependency initialization")
	}
	a.runtimeInitialized()
	if !a.runtimeAvailable() {
		t.Fatal("runtime is unavailable after dependencies are initialized")
	}
}
