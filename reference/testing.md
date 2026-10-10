# Local agent tests and model selection

## Default mocks

`agenttest.New(t, factory)` prepares an independent
`github.com/airlockrun/goai/testutil.MockModel` for each registered `CapText` or
`CapVision` slot, plus the unnamed default text slot. It prepares models before
startup, including `OnStart` hooks. App code declares its model slots and uses
the normal SDK getters; it does not select test providers.

Default mocks answer any request with `Hello` and usage of 10 input / 5 output
tokens. Configure meaningful task responses explicitly. `MockModel("")` addresses
the default text model used by `GenerateText`/`StreamText` when Model is unset.
Unknown and non-streaming slots fail rather than resolve another model.

```go
env := agenttest.New(t, newAgent)
mock := env.MockModel("conversion")
specific := "Convert the orders table"
err := mock.Configure(testutil.MockConfig{
    ID: mock.ID(), // Configure retains the mock's immutable identity.
    Default: &testutil.MockResponse{Text: "Default response"},
    Rules: []testutil.MockRule{{
        LastUserText: &specific,
        Response: testutil.MockResponse{Text: "Specific response"},
    }},
})
if err != nil {
    t.Fatal(err)
}
```

Rules are checked in order. `Match func(*stream.CallOptions) bool` can inspect
messages, tools, response format, reasoning, or provider options. `MockResponse`
supports text, tool calls, usage, and explicit stream events. `Responses` supplies
a fallback sequence for multi-turn task tests. `Requests()` returns captured
request snapshots. Configure can run concurrently with Stream; environments and
slots do not share mutable mock state.

For startup calls, use `agenttest.NewWithOptions(t, factory, options)` with
`Options.Mocks map[string]testutil.MockConfig`. Keys name slots; `""` names the
default. An omitted mock ID gets a slot identity. `Options.Limits` overrides mock
compaction limits; defaults are Context: 100000, Output: 4096. Other modalities
require an explicit local adapter; the actual local host has no canned responses.

The SDK proxy records model requests and callback headers through
`env.Airlock.Requests()`. Every streaming request dispatches to its configured
canonical mock or explicitly selected live model. Configure per-slot responses
with MockConfig; MockResponse.Events supplies protocol-specific event sequences.
MockConfig.Stream supplies a custom streaming function for cancellation, setup
errors and request-boundary tests, using the same request capture surface.

## Explicit live selection

A test package opts into CLI configuration once, before flag parsing:

```go
var modelOptions = agenttest.RegisterFlags(flag.CommandLine)

func TestConversion(t *testing.T) {
    env := agenttest.NewWithOptions(t, newAgent, *modelOptions)
    mock := env.MockModel("conversion")
    // Configure ordinary deterministic responses here. If conversion is live,
    // this mock stays dormant; the same test setup works in both modes.
    _ = mock
}
```

```sh
go test -run '^TestConversion$' -count=1 -args \
  -agenttest.model=conversion=work/openai/gpt-5.4
```

`agenttest.model` is repeatable. Only named selections go live; other slots remain
mocked. `@default=slug/provider/model` selects the unnamed text slot. A registered slot
literally named `default` is addressed as `default=slug/provider/model`. Duplicate,
malformed, unknown, and non-streaming selections fail. Live selections require
an existing named account in Sol's private `sol/config.json`.
Use a scoped `-run` when other tests construct apps that do not declare the chosen
slot. Programmatic selections use `Options.Models map[string]string` with `""`
for the default. Each reference selects its own authentication binding.

RegisterFlags performs no credential or network lookup and registers nothing
globally on import. Plain New never consumes these flags. NewWithOptions resolves
explicit live selections through Sol's `provider.ResolveLocalModel`, including
its model capability limits and saved credential/API-key policy. Resolution runs
only when the environment requests a live slot, before app startup. Missing
credentials or unsupported models/auth fail; no interactive login, stored default
model, alternate provider, or mock fallback is selected. Live limits come from
Sol's resolver. A live run may refresh credentials at the provider's request
boundary. Ordinary tests use mocks and read no credentials or live endpoints.

## Runnable registered-task example

The external-package test in
[`agenttest/examples/csv/app_test.go`](../agenttest/examples/csv/app_test.go)
executes an app-owned conversion task with real Deno and an actual CSV artifact.
Its [app factory](../agenttest/examples/csv/app.go) declares the `conversion`
text-model slot and a private typed `write_csv` tool. No model or provider is
bound in app code. The tool receives structured string cells and uses Go's
`encoding/csv` to write `converted.csv` into the test's `t.TempDir()`.

Run from the AgentSDK module directory, with Go 1.26.9+ and Docker available:

```sh
go test ./agenttest/examples/csv -run '^TestConversion$' -count=1 -v
```

The default run reads no model credentials and makes no live model calls. The
test explicitly calls `RegisterFlags(flag.CommandLine)` once and passes the
returned options to `NewWithOptions`. It always configures the canonical mock
with a two-response sequence: `run_js` invokes `tools.write_csv`, then `complete`
returns the typed output. The model slot policy decides whether that mock is
active or dormant. Real app dispatch, the task runtime and real Deno run in
both modes.

To run that exact test with an explicitly selected live Codex model, prepare
credentials using Sol's explicit authentication flow separately, then run:

```sh
go test ./agenttest/examples/csv -run '^TestConversion$' -count=1 -v -args \
  -agenttest.model=conversion=work/openai/gpt-5.4
```

This command switches only the `conversion` slot. Prepare the account with
`sol auth login work/openai --method codex`. Model selection does not authenticate the app
with Airlock, configure HTTP/MCP resources, or change other model modalities.
No build tag or source change is needed. Live credentials and model support must
be present; missing configuration fails rather than using the mock. Do not append
these flags to an unrestricted `go test ./...`: other packages may not register
them or may construct apps without the selected slot.

The example:

1. Uses a checked-in source-layout fixture under `testdata/app` so startup can
   discover `db/migrations`. It needs no application database tables.
2. Starts the SDK app with the usual disposable PostgreSQL dependency, then
   configures its `conversion` mock through the public API.
3. Selects a real executor: a supplied build-scoped transport when
   `AIRLOCK_TEST_EXECUTOR_URL`/`AIRLOCK_TEST_EXECUTOR_TOKEN` are configured;
   otherwise `TEST_JSEXEC_IMAGE`, or an explicitly built
   `agentsdk-csv-example:local` image using `jsexec.BuildImage`.
4. Lets RunAgent generate an application-owned admin scope with a fresh run UUID
   and invocation token bound to the environment's app identity. There is no
   human user or initiator. The token is a local invocation fixture, not a
   credential for hosted Airlock APIs.
5. Calls `RunAgent(t, ctx, env, handle, input, RunOptions{ExecutorFactory: factory,
   MaxSteps: 8, Timeout: 2 * time.Minute})`.
6. Checks typed output completion and parses the actual CSV file. Headers, row
   counts, commas, quotes, embedded newlines, Unicode and all original cell
   values must match. It does not assert exact model prose, tool-call IDs or
   model-step counts, so the same assertions apply to live execution.

Verbose output includes `CSV artifact verified: .../converted.csv (3 data rows)`.
The temporary artifact is removed during test cleanup. The file is produced by
the native private tool; Deno has no direct filesystem access. It is not an SDK
logical storage path or hosted download. Real SDK file storage, XLSX parsing and
external service access would require their own app tools/backends.

`TEST_DB_URL` can select a disposable preconfigured PostgreSQL database. With
neither that variable nor Docker available, the environment follows agenttest's
database-provider skip behavior. A configured database still needs an executor
image/Docker or the explicit executor transport. No fake JavaScript executor is
substituted by this example.

## Registered tasks and chat

`agenttest.RunAgent` executes a registered leaf task through the shared task
runtime, using its declared ModelSlot and the environment's slot policy. It
requires an explicit executor factory. Omitting Scope generates an
application-owned scope; a supplied scope must match the environment's app.
Private tools go through the real authenticated app handler. `Options.Storage`
enables SDK-owned local object storage and attachments as described below.
Other unsupported host services require an explicit Backend or fail; this helper
does not supply OAuth resources, schedules, bridges, or child scheduling.
New/NewWithOptions runs `localruntime.Host`: normal AgentHandle
Start/Get/List/Wait/Cancel/Continue methods address its durable scheduler.
Use `Options.ExecutorFactory` for task JavaScript; `Options.Resources`,
`Options.Discovery` and `Options.Backend` inject explicit resource fixtures.
Low-level NewMockAirlock and isolated responders support protocol-client tests.
See [local-development.md](local-development.md) for the capability matrix.

`env.Chat(ctx, scope, chatruntime.Input{Model: model, ...})` already accepts an
explicit model independent of registered task slots. Supply model limits, session
storage, an event sink, capabilities, a backend, and a positive step budget.
Direct-tool mode needs no JS executor. `agenttest.MemoryStore`, `agenttest.Events`
and `agenttest.Executor` provide conversation fixtures and real Deno execution.
See [the chat runtime contract](../chatruntime/README.md).

New and NewWithOptions share the test database/migration lifecycle. TEST_DB_URL
selects an explicitly supplied disposable database; otherwise they provision
throwaway PostgreSQL. Tests validate migrations with an up/down-to-zero/up cycle.

## Isolated file storage and model images

```go
files := agenttest.NewFileStorage(t)
_, err := files.WriteFile(t.Context(), "evidence/page.png", pngReader, "image/png")
if err != nil { t.Fatal(err) }
options := *modelOptions // Explicit RegisterFlags options, or Options{} for mocks.
options.Storage = files
env := agenttest.NewWithOptions(t, newAgent, options)
```

The application still declares directories and uses its ordinary `Agent` methods.
`NewFileStorage(t)` owns a disposable `t.TempDir` namespace and test cleanup.
`FileStorage.WriteFile` seeds deliberate fixtures through an `io.Reader`;
`OpenFile` streams an object for assertions. Neither method interprets an object
path as a host pathname. To import an external test PDF, explicitly open the
chosen fixture in test code and pass its reader to WriteFile.

New/NewWithOptions creates isolated storage; `Options.Storage` selects a seeded
store. The shared local-host storage endpoints
implement real write, open/read, stat, list, range, copy and delete over this same
store. Missing objects return not-found; they do not use canned content. Atomic
replacement preserves prior content when a stream fails or exceeds 80 MiB.
Canonical app-local paths reject traversal, absolute paths and symlinks; an
`os.Root` also confines filesystem operations. The fixture is not a production
file catalog and does not implement FileRef/indexing, home-file ACLs or sharing.
Configured upload logs record metadata rather than duplicate stored file bytes.

`RunAgent` and `env.Chat` supply `air.attachToContext` from this storage when
configured, with ordinary SDK directory/scope authorization. App-local storage
operations dispatch through the authenticated SDK handler. Other platform
capabilities still require an explicit Backend. Application tasks use internal
file access: declare task evidence with `AccessInternal`; that access is separate
from human public/user/admin policies. Generic task file operations do not gain
access to ordinary human directories. Native Go private tools may use their
trusted app-owned file API after validating their domain's admitted input.

Registered test-slot models automatically resolve `s3ref:` attachments at the
provider boundary using the configured store. `env.Chat` also wraps its supplied
model when Storage is configured. Messages/checkpoints retain object references;
only the model-call copy receives inline native GoAI FilePart image content.
This is not base64 returned as text. PNG/JPEG/GIF content is validated, bounded
at 5 MiB per image and 32 MiB of materialized attachments per request. Non-image
attachments are capped at 16 MiB each. Missing stores, objects, undeclared roots,
invalid content, and MIME mismatches fail before calling the model. No provider
or local-file fallback is used to resolve these references.

The runnable `agenttest/examples/images` test demonstrates seeding a PNG,
registered task execution through real Deno and automatic attachment resolution.
Mock execution verifies exact PNG bytes in the model FilePart and references in
checkpoints. Explicit live execution audits actual Codex `input_image` content,
retaining only an exact image hash, never credentials or complete request bodies:

```sh
TEST_JSEXEC_IMAGE=agentsdk-csv-example:local \
go test ./agenttest/examples/images -run '^TestStoredImage$' -count=1 -v -args \
  -agenttest.model=image_probe=work/openai/gpt-6.1-sol
```

Without model flags the same test uses its configured canonical mock.
New/NewWithOptions supplies isolated real storage, and Options.Storage selects
deliberately seeded objects. Low-level NewMockAirlock protocol fixtures are not
model attachment data. Supply an explicit executor image or let the example build
the shared recipe; build-scoped executor transport is supported as well.
