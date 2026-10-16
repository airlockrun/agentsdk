# Deterministic local tests and real remote integration

## Local unit tests

`go tool air test` runs `go test -p=1 -count=1 ./...` without starting a database,
Docker, Deno or model. `air build` includes the same deterministic test step after
generation. Default app source/tests cannot import SDK `agenttest`, `localruntime`
or testcontainers: those imports fail before toolchain/build/test execution.

Test domain logic through its typed dependencies. Use in-memory repository
fixtures for domain state, `httptest` recorders for direct handler rendering,
explicit model fixtures for model-dependent code, and `fstest.MapFS`/byte readers
for input files. These tests do not prove host admission, migrations, SQL query
correctness, file ACLs or durable scheduling.

Offline composition tests call `newAgent().Manifest()` and assert declarations,
schemas and registration. Do not call Agent.Start/Serve/Handler locally: the real
runtime requires Airlock credentials/database/sync and is tested remotely.

### Model mocks

Inject `github.com/airlockrun/goai/testutil.NewMockModel` directly into the domain
service's `stream.Model` dependency. Supply an explicit scripted MockConfig;
rules/response sequences and recorded requests let the test assert domain behavior.
There is no automatic slot fallback, live model/account lookup or local task loop.
Local tests never select a real provider by an environment credential or
`-agenttest.model` flag. Live evaluations require an explicit remote suite and
host-authorized provider/budget configuration.

### Caller fixtures

Use `agentsdk/testutil.WithCaller` for direct SDK-aware domain/handler calls:

```go
ctx := testutil.WithCaller(t.Context(), wire.Caller{
    Kind: "application", Access: wire.AccessAdmin,
    Origin: wire.CallerOrigin{Interface: "application", Execution: "startup"},
})
if _, human := agentsdk.CallerFromContext(ctx).User(); human {
    t.Fatal("application work must not receive an owner identity")
}
```

This fixture validates/copies metadata without runtime work. It cannot authorize a
remote HTTP request or cross the network as a credential. Remote tests use verified
user tokens and host-owned run/receipt/fence admission, not forged caller headers.

## Real remote app operations

Create/update a personal running app with `air preview`; then request HTTP,
invoke registered tools and start/get/wait/cancel agents through the actual
host. `agentsdk/remote.Client` requires explicit origin/logical app and
token callback/HTTP client. Target defaults to production; previews require a
concrete deployment ID/name. HTTP/tool/agent-start require an explicit observed
source revision. `X-Airlock-Expected-Source-Revision` is checked by host admission
before callbacks. Runner calls additionally use the server's pinned runtime/config.

Native jobs and app-origin agent starts retain application actors; operator
starts record their verified initiator. Direct user resources require independent
resource use; target binding additionally requires app/target edit rights. Binding
is server-side and has no assign permission. Static HTTP fixture responses come
from the whole immutable candidate source. They never fetch laptop per-file paths
or fall back to a real resource on a fixture miss.

## Candidate-owned integration suites

Put integration Go tests behind `//go:build airlockintegration`, with their
fixture data in `testdata`/declared source roots in the whole candidate source
bundle. Remote suites are explicit opt-in; local test/build commands do not run
them. The host compiles one test binary per package from the exact deployed
revision and preserves each package's working directory/testdata layout.
Tests run in a separate suite runner against the selected preview's existing
app runtime, database, files and resource bindings. Airlock does not create a
fresh app runtime or clone/reset state for each test or suite. Tests and
interactive preview use share state, so test mutations remain visible there.
The preview's app state is isolated from production; bound real resources retain
their own external state.

Run `air test --remote NAME --target preview --revision UUID --suite integration
--wait`. Admission packages tests from that exact deployed revision, not a laptop
executable. `air test status|cancel --run UUID` uses the same remote/target/revision.
Wait cancellation stops observation only. Assertions, panic, timeout, interruption
and empty suites fail; a missing host capability fails as an HTTP error.

`remote.TestClientFromEnvironment()` requires host-supplied `AIRLOCK_TEST_API_URL`
(exact `/api/test-runs/UUID` root), `AIRLOCK_TEST_TOKEN` (opaque capability),
`AIRLOCK_TEST_APP_ID`, `AIRLOCK_TEST_DEPLOYMENT_ID`, `AIRLOCK_TEST_SOURCE_REVISION`
and `AIRLOCK_TEST_RUNTIME_GENERATION`. The client exposes `Request`, `InvokeTool`
and `StartAgent`/`GetAgent`/`WaitAgent`/`CancelAgent` within that test-run scope,
not ordinary login/binding/deployment/resource management. The host checks the
pinned source revision, runtime generation and configuration revision on runner
calls. Missing coordinates fail, with no account discovery or simulator fallback.

Default `go test ./...` runs all pure unit/model/caller/protocol tests. Real PG,
Deno and image checks require `-tags=airlockplatformintegration` and explicit
platform infrastructure (`AIRLOCK_PLATFORM_TEST_DB_URL` for migration locks).
Requested platform tests fail if infrastructure is absent; they do not silently
skip unit coverage based on Docker availability.

See [remote development](remote-development.md) for supported CLI operations,
fixture-selection revision requirements and source setup-barrier limitations.
