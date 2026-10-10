# Local app execution

`go tool air run --local` compiles and runs the app's ordinary `Serve` entrypoint
against `localruntime.Host` using `airlock.app-runtime.v3`. App HTTP handlers,
tools, private tools, database and file APIs use their normal SDK implementations.
Hosted and local reasoning share `agentruntime`; JavaScript shares the packaged
`jsexec` supervisor, protocol and Deno recipe. The public SDK imports no private
Airlock packages.

## Configuration and startup

Run `go tool air build` to prepare generated assets. Docker is required for Deno.
Without `databaseEnv`, the CLI automatically starts one ephemeral pgvector
PostgreSQL 17 container for this app. Database bytes persist in a host bind mount;
there is no shared PostgreSQL manager and no database sharing across apps.

Create `.airlock/local/runtime.json`:

```json
{
  "user": {
    "id": "dcb75da0-51dc-46e4-bf5e-8f7aa3988b0f",
    "email": "developer@example.test",
    "displayName": "Local developer",
    "platformMember": true
  },
  "models": {
    "@default": "work/openai/gpt-5.4",
    "reasoning": "personal/openai/gpt-5.4"
  },
  "executorImage": "ghcr.io/airlockrun/airlock-js-executor@sha256:460592372756ec350e30c3c7701481733b31368d42565eecc709bb9619090cd6"
}
```

`@default` selects the unnamed text model. Configure every declared text/vision
slot by its exact slug. Unknown or missing slots fail explicitly. Values are
`slug/provider/model` references, never credential-bearing objects. Sol's private
OS user configuration (`sol/config.json`) owns named entries and their inline
API keys or account-scoped Codex credentials. Prepare entries separately with
`sol auth set-key personal/openai` or `sol auth login work/openai --method codex`.
The shared Sol resolver requires the exact named entry; it never initiates login,
selects an environment-created profile, inherits Sol's default model, or
substitutes mock responses. Different slots can select independent accounts of
the same provider. The provider/model suffix must have catalog limits. Optional
API-key account `baseURL` lives in the private Sol config, not the app config.

SDK builds pin the published Sol v0.1.16 account-selection API. Workspace builds
overlay the sibling source through go.work; standalone builds resolve the published
module without replacement directives.

From the app module root:

```sh
go tool air run --local
```

- `--config PATH`: explicit JSON, default `.airlock/local/runtime.json`.
- `--addr IP:PORT`: loopback developer listener, default `127.0.0.1:0`. The OS
  selects a free port and the CLI prints its URL. An occupied explicit port fails
  rather than switching silently. The private app socket stays reserved through
  process startup using inherited socket activation (Linux/macOS).
- `--build-executor`: explicitly build the embedded production Deno recipe under
  the configured image name. Docker and the SDK Go toolchain are required.
  Use a local tag such as `agentsdk-jsexecutor:local` with this source-development
  option, not an immutable published digest. A compatible public image needs no
  build flag. Executors allocate lazily for `run_js`.

The selected image is pulled when missing and checked against the embedded
supervisor/protocol/runtime assets. The immutable v0.7.0 artifact above is
compatible with this SDK source despite its different SDK-version label. Image
tags are Airlock release versions, never derived from SDK semver. Unknown runtime
metadata or changed assets fail with an instruction to select a compatible image
or build a development image. Deno uses network-none, read-only, resource-limited
Docker containers and framed stdin/stdout; no Deno TCP port is published. Native
Deno execution is not a local-mode option.

### PostgreSQL data and external databases

The default host data directory is `.airlock/local/postgres/`. Optional
`"postgres": {"dataDir":"/absolute/private/app-pg"}` selects another dedicated
directory. Relative paths resolve from the app module root. Paths inside the app
must remain beneath `.airlock/local/` so source exports cannot contain database
files or credentials. External directories are also supported. Directories must
be owned by the invoking UID/GID with mode 0700; the CLI never changes ownership
or recursively deletes data. `pgdata/` holds PostgreSQL system files and
`credentials.json` holds mode-0600 generated administrator/app credentials.

Each container runs with the invoking UID/GID, mounts that app's `pgdata/`, and
binds PostgreSQL to one Docker-assigned `127.0.0.1` port. The app role is the owner
of its app database, with no superuser, role-creation, database-creation or
replication privileges. pgvector is provisioned by the local administrator.
The pinned managed image can be overridden with `postgres.image` only using an
immutable digest; changing the persisted image requires explicit migration.
The local CLI's inherited-socket handoff supports Linux/macOS. Managed bind mounts
require a non-root invoking user; root users on these platforms must select an
external database. Other platforms fail explicitly at socket activation.

A lifetime file lock prevents two processes mounting the same data directory.
Shutdown first drains the app and local workers, then stops its verified owned
PostgreSQL container; Docker auto-removes the container while the host data stays.
After a coordinator crash, startup can remove only the exact stale container
whose app, path, UID/GID, image and mount ownership match. Unrecognized containers
are never stopped. A missing/corrupt credential record or incompatible PG version
fails without resetting the database.

To use an existing dedicated database instead, add
`"databaseEnv":"LOCAL_APP_DATABASE_URL"` and set that variable to its DSN.
Do not also configure `postgres`. An explicitly selected but unset variable fails
immediately; it never falls back to a managed container. External databases are
not started, stopped or reset by the CLI. App migrations use normal startup up
migrations, not the test-only down/reset cycle.

The private app and callback listeners use separate ephemeral loopback ports.
The child receives fresh local credentials and the standard runtime environment.
`AIRLOCK_MIGRATIONS_DIR` selects the absolute source `db/migrations` directory.
Normal startup applies migrations without test down/reset cycles. Inherited
Airlock credentials, host URLs and migration-down commands are removed from the
child environment. The app runs in its source directory with source-relative
runtime files. `run` does not execute `setup.sh` or generate frontend assets.

For registered `EnvVarHandle` values, optional `"envVars": {"region":"LOCAL_REGION"}`
maps a declared slug to a process environment-variable name. Values and secrets
stay outside the JSON. Explicit mappings require an existing environment value;
unmapped slots retain the SDK's declared-default/missing-value contract.

The browser uses the explicitly configured local developer as an admin caller.
The host supplies caller/run/delivery metadata; internal invocation, webhook and
job endpoints are private. To invoke a native tool through the actual SDK handler:

```sh
curl --fail-with-body -X POST "$LOCAL_APP_URL/__air/local/tools/my_tool" \
  -H 'Content-Type: application/json' -d '{"value":"example"}'
```

## Durable state

`.airlock/local/runtime/` contains the stable app UUID, control-state journal,
compiled binary and object store. The scaffold excludes `.airlock/local/` from
Git and source archives. Provider credentials are not written there.

Journal transactions use file locks, reload shared state, fsync and atomically
publish snapshots. Files use a persistent catalog over immutable SHA-256 content;
an open reader retains its content version across overwrites. Multiple local
hosts can share state/storage on a filesystem supporting locking and atomic
rename. The CLI permits one coordinator per app workspace; separate apps can run
concurrently with separate PG containers and automatic ports. The app database
remains PostgreSQL.

Native `AgentHandle.Start/Get/List/Wait/Cancel/Continue` uses durable IDs, exact
definition contracts, request replay checks, transcript/checkpoint transactions,
owner-fenced worker leases and reasoning budgets. Application-owned tasks have
no human user or substituted owner. Declared one-level children use durable calls
and waits; parking releases the worker and JavaScript realm. Recovery rotates
callback receipts, preserves the task ID/checkpoint and reports unknown outcomes
for interrupted generic effects. Completed checkpoints settle without reasoning.

Typed jobs dispatch to the real SDK `/job/{name}/{version}` handler with contract
schemas, scheduled timestamps, progress and attempt fences. Cron occurrences use
deterministic IDs under the journal lock. Interrupted native job effects are
reported as failed/unknown rather than automatically redelivered. An explicit
SDK `retry` response requests another attempt within the configured limit.
Shutdown drains workers and the child while retaining state and PostgreSQL data.

## Bound Airlock resources

No-auth HTTP/MCP declarations use their fixed manifest endpoints directly through
the local HTTP/MCP transports. They need neither an Airlock login nor a resource
binding. No-auth MCP schemas are discovered at local CLI startup and retain their
canonical server/tool identities. Tests inject `Options.NoAuthResources` and
`Options.Discovery`; ordinary `agenttest.New` never contacts declared external
endpoints implicitly.

Optional `"resourcesRemote": "my-remote"` selects an existing workspace remote.
Prepare `go tool air login` separately. The local developer UUID must match
authenticated `/api/v1/me`. Calls use the remote app's current declared and bound
needs; local declarations do not create or rewrite remote bindings. Synchronize
remote need contracts explicitly.

This is the authenticated CLI login session, not a hosted developer-chat or
toolserver session. The remote Airlock binary must include the local-resource
admission/gateway endpoints. Local execution requires SDK v0.8.1-alpha.9 or newer.
The `run` command starts local dependencies and the app; it does not deploy a host.

The CLI host exchanges its live developer session for a 15-minute
`local_resources` credential bound to the exact session, user and remote app.
Only `/api/local/agents/{appID}/integrations/*` accepts it. Renewal requires the
same developer session and live app-admin gate; changing logins requires explicitly
restarting the local runner. The app and Deno receive neither this credential,
the CLI session token, nor upstream credentials. Airlock checks live session,
expiry, app-admin access and current binding policy on every request and ignores
local caller/run assertions. Human calls select the authenticated developer's
bindings. Application-owned calls use `/integrations/application/...` with no
human binding identity, so only shared bindings are available. Application tasks
reject per-user needs. Models, files, databases and jobs stay local.

Human run/job records retain their original remote session coordinate. Restarting
with another session cannot lend that session's per-user credentials to persisted
human work. Fixture resource providers supply `Options.ResourceSession` explicitly.

This adapter uses the integration API's bounded response envelope (20 MiB),
including app `RequestStream`; it is not an unbounded download transport.
Application-owned resource calls depend on the developer's live session and
app-admin grant; this is development authority, not autonomous deployed app authority.

## Shared test runtime

`agenttest.New`/`NewWithOptions` starts this host with isolated state/storage,
disposable PostgreSQL (or explicit `TEST_DB_URL`) and fake models. `env.Airlock`
retains request recording. Low-level `NewMockAirlock` supports protocol-client
fixtures. `Options.ExecutorFactory`, `Options.Resources`, `Options.Discovery`
and `Options.Backend` inject explicit boundaries. Ordinary tests discover no live
model credentials; see [testing.md](testing.md) for explicit opt-in and diagnostic
`RunAgent`.

Native task lifecycle, child scheduling, jobs and storage use the same
`localruntime.Host` implementation in the local CLI and agenttest. Diagnostic
`RunAgent` is a leaf-only helper with attempt-local persistence and budget fixtures;
it uses the shared `agentruntime` loop and app/executor transports but does not
exercise the durable scheduler. Hosted Airlock also uses `agentruntime`, SDK v3
dispatch and the shared executor factory; its PostgreSQL scheduler, execution
admission and file-catalog adapters remain hosted implementations.

```sh
GOWORK=off go test ./...
JSEXEC_DOCKER_TEST=1 GOWORK=off go test ./agenttest \
  -run '^TestSharedLocalRuntimeDenoPrivateTool$' -count=1 -v
GOWORK=off go test ./cmd/air \
  -run '^TestLocalCLIStartsActualAppWithFixtureDependencies$' -count=1 -v
```

The CLI fixture runs a real source app with disposable PostgreSQL and an explicit
fake provider URL/key, with no billable model request. The Deno fixture uses fake
reasoning responses and actual private tool, database and file effects. Recovery
fixtures reopen persisted state and reject duplicate interrupted effects.

## Capability matrix

| Capability | Local | Hosted |
| --- | --- | --- |
| HTTP routes, assets, native tools | Actual SDK handler; explicit developer | Actual SDK handler; verified host caller |
| App DB/migrations | Per-app ephemeral Docker pgvector, persistent host bind mount or explicit external DSN; source migrations | Scoped role/schema; packaged migrations |
| Registered internal/scoped files | SDK policy; durable local content/catalog | SDK policy; host file catalog/object store |
| Task agents/declared children | Shared `agentruntime`; filesystem scheduler | Shared `agentruntime`; PostgreSQL scheduler |
| Deno callbacks | Packaged `jsexec`; explicit image | Packaged `jsexec`; leased transport |
| Typed/scheduled jobs and crons | Durable local journal; SDK job handler | PostgreSQL scheduler; SDK job handler |
| Text/vision model slots | Explicit local provider bindings | Host model broker |
| Bound HTTP/MCP | Explicit adapter; live remote developer gates | Host credential broker and admitted origins |
| No-auth HTTP/MCP | Fixed manifest endpoints; direct local transports | Fixed manifest endpoints; host outbound transport |
| Registered environment values | Explicit environment bindings; SDK defaults/patterns | Configured host values and SDK defaults/patterns |
| Non-streaming model slots | Unsupported; declaration fails startup explicitly | Host model broker |
| Search/fetch, topics, sealing, connector operations | Not supplied by the CLI; calls fail explicitly | Host service implementations |
| Human chat UI, bridges, deployment, home-file sharing/indexing, tenant administration | Host control-plane features | Airlock control plane |

The actual local host never returns successful canned responses for unsupported
services. Its journal is separate from the hosted PostgreSQL schema, and its
object catalog does not implement the hosted Files sharing/indexing API.
