# JavaScript Executor

**Status: the permission-isolation gate passes on the pinned Deno runtime.**
`jsexec` supplies a transport-neutral client, a Go supervisor, and a shared
production/test container recipe. `cmd/jsexecutor` is the container entrypoint.
There are no Airlock imports or third-party Go dependencies in this package.

## Integration

```go
type Session interface {
    Execute(ctx context.Context, code string, callback Invoker) (Result, error)
    Close() error
}

type Invoker interface {
    Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

func NewClient(transport io.ReadWriteCloser, options Options) (*Client, error)
func NewDockerSession(ctx context.Context, image string, options Options) (*DockerSession, error)
func BuildImage(ctx context.Context, tag string) error
func Serve(ctx context.Context, transport io.ReadWriteCloser, config SupervisorOptions) error
```

For an SDK test or a Docker-CLI launcher:

```go
if err := jsexec.BuildImage(ctx, "local/jsexecutor:test"); err != nil {
    return err
}
options := jsexec.Options{
    Limits: jsexec.DefaultLimits(),
    Bindings: []jsexec.Binding{
        {Name: "file_read", Path: []string{"air", "fileRead"}},
    },
}
session, err := jsexec.NewDockerSession(ctx, "local/jsexecutor:test", options)
if err != nil {
    return err
}
defer session.Close()
result, err := session.Execute(ctx,
    `globalThis.report = await air.fileRead("reports/current.json"); return report;`,
    invoker)
```

The host must supply a non-nil `Invoker`, validate arguments and authorization
for every capability, and return valid JSON. Arguments are a JSON array of
positional arguments. `InvokerFunc` adapts a Go function. Callbacks run
concurrently up to the configured limit and **must honor context cancellation**.
The client cancels and waits for accepted callbacks before `Execute` returns.
Go cannot forcibly terminate a callback that ignores its context.

An Airlock Docker-API launcher can create the same image with the container
policy below, demultiplex Docker attach stdout, and pass a full-duplex stream to
`NewClient`. Do not use a TTY. `Close` on the stream must unblock concurrent reads
and writes; the launcher owns container removal. `NewDockerSession` performs
that cleanup itself and exposes `ContainerID` for observation. Its creation
context owns the container's lifetime.

`BuildImage` requires an installed Go 1.26+ toolchain and Docker CLI. It compiles
only embedded, dependency-free supervisor sources with `GOWORK=off`,
`GOTOOLCHAIN=local`, `CGO_ENABLED=0`, and Linux as the target OS. It builds for
the calling process's architecture. The build context contains only that
binary and the embedded recipe/runtime scripts. No checkout, credentials,
Airlock release, downloaded Go dependencies, or working-tree copy is required.

## Execution Semantics

- Code is a strict async function body. Use `await` and explicit `return`.
- Store retained values on `globalThis`. Local `var`, `let`, and `const`
  declarations belong to the function invocation and are not retained.
- Globals persist throughout an uninterrupted session, including recoverable
  JavaScript errors. State is ephemeral and session-affine, not shared between
  replicas or persisted in a database.
- Session loss, timeout, cancellation, protocol failure, or resource termination
  is terminal. There is **no automatic recreation or code replay**.
- Overlapping `Execute` calls return `ErrBusy`; they are not queued.
- Capability bindings and `invoke(name, ...args)` capture their invocation.
  Saving one on `globalThis` does not rebind it to a later host callback.
- Once the snippet's returned promise settles, new broker calls are rejected.
  Already accepted calls drain before completion. Fire-and-forget calls are
  therefore bounded and drained, not silently left running.
- `Result.Output` is JSON; `Undefined` distinguishes absent output from JSON
  `null`. Cyclic and BigInt return values produce a JavaScript exception.
- The injected lexical `console.log/info/warn/error` produces ordered typed
  `Logs`. A `*Exception` can accompany partial logs. Raw global console output,
  including console calls in imported modules, is discarded with child stdio.
- `air.log(...values)` is a synchronous local alias of the injected `console.log`,
  with the same log bounds and invocation lifetime. It never calls the broker.
- Optional `Options.User` supplies a frozen lexical `user` object containing only
  `id`, `email`, and `displayName`; nil exposes `null`. The host derives it from
  authenticated run attribution. It is display context, not authorization.
  `user` and `air.log` are reserved binding paths. Local tests supply the same
  claims through `agenttest.ExecutorConfig.User`.

Background work is not a supported session-persistence mechanism. Worker use,
unreferenced timer controls, `Atomics.waitAsync`, outstanding timer/resource
accounting, and additional native worker threads make reuse conservative and
can return a terminal `ResourceError`. A completed worker operation may still
terminate the session. Hung snippets and workers are killed by the supervisor
deadline or cancellation; no attempt is made to replay state.

The resource check uses the pinned Deno native leak tracer to include
unreferenced `node:timers/promises` timers that Node's active-resource list
omits. Access to exposed low-level Deno core controls marks the invocation
non-reusable because those controls can disable accounting. These are lifecycle
checks, not replacements for Deno permissions or container resource isolation.
They do not purport to await every arbitrary user-created JavaScript Promise.

## Isolation

```text
host capability broker
    <-> bounded framed Docker attach stdin/stdout
Go supervisor inside network-none container
    <-> bounded framed Unix socket
trusted Deno controller isolate
    <-> private MessagePort
permission-denied snippet worker and its descendants
```

The Deno subprocess gets `/dev/null` for stdin, stdout, and stderr. Direct
`Deno.stdout`, Node stdio, or default worker `postMessage` traffic is not
control traffic and cannot forge attach frames. The controller never evaluates
snippet code. It has read access only to the trusted worker bootstrap and the
temporary Unix socket, write access only to that socket, and a socket-scoped
network grant. The listener is closed and unlinked after the controller connects.

Snippet workers receive `deno: {permissions: "none"}`. Network, filesystem,
environment, subprocess, FFI, and permission escalation remain restricted by
stock Deno, including operations reached through imported Node modules or
descendant workers. Data modules, built-in modules, `Function`, `eval`, and
workers are permitted language/runtime facilities, not security failures.
Remote and npm resolution are disabled in the controller launch policy.

The controller and Go supervisor validate invocation and call state. The host
client independently validates capability names, argument shape and size,
unique call IDs, concurrent/total call limits, result/log sizes, and completion
with no pending callbacks. Unknown capabilities never reach the host `Invoker`.
Malformed, stale, duplicate, oversized, and out-of-state traffic closes the
session. Invocation IDs are correlation identifiers, not authentication tokens.
Host authentication and authorization belong outside this container.

Captured operations and null-prototype records protect bootstrap bookkeeping
from ordinary prototype mutation. No source regex or deleted-global list is
claimed as a sandbox boundary. The native runtime, trusted bootstrap/controller,
supervisor, host broker validation, and container kernel isolation are all part
of the trusted computing base. Stock-runtime vulnerabilities are not solved by
this package; keep the digest and hostile tests under review.

## Resource Policy

`NewDockerSession` uses these required container settings:

| Setting | Value |
| --- | --- |
| Network | `none` |
| Memory and memory-plus-swap | 256 MiB each, no additional swap |
| CPU quota | 1 CPU |
| PIDs | 128 |
| Root filesystem | Read-only |
| User | `65532:65532` |
| Capabilities | All dropped |
| Privilege escalation | `no-new-privileges` |
| Writable storage | `/tmp`, 32 MiB tmpfs, `noexec,nosuid` |
| Docker logging | Disabled |

Do not mount credentials, the Docker socket, host storage, or a network-enabled
sidecar into this container. The child environment is explicitly constructed
with only a disposable cache path and update-check setting. `Serve` is an
in-container API, not a host-process sandbox constructor.

`DefaultLimits()` supplies an explicit policy; zero-value limits are rejected:

| Limit | Default |
| --- | --- |
| Frame | 1 MiB, also an absolute protocol ceiling |
| Code | 256 KiB |
| Output / broker argument / broker response | 256 KiB |
| Logs | 256 entries and 64 KiB of message text |
| Concurrent broker calls | 8, excess rejected rather than queued |
| Total broker calls per invocation | 256 |
| Execute deadline | 10 seconds, including client-side startup/draining |
| Idle deadline | 60 seconds |
| Absolute supervisor lifetime | 10 minutes |
| Invocations per supervisor | 4096 |

The supervisor has an independent deadline watchdog that closes blocked IPC
and attach I/O and kills Deno. Container memory/CPU/PID limits cover the
supervisor, controller, snippet worker, descendant workers, module loading,
serialization, and native runtime allocations together.

## Attach Protocol

Each frame is a four-byte unsigned big-endian length followed by one UTF-8
JSON object. No line protocol, TTY, Docker multiplex header, or child stdout
belongs inside a frame. Oversized lengths are rejected before allocation;
truncated frames, unknown JSON fields, and trailing JSON are errors.

The client sends `execute` with `id`, `code`, and `options`. Options are fixed
for the session and repeated verbatim on subsequent executions. The supervisor
sends `call` with `id`, positive `call`, capability `name`, and JSON-array `args`.
The client responds with `reply`, matching `id`/`call`, and either JSON `value`
or nonempty `error`. Completion is `done` with `result`, optional `exception`,
and `terminal`; `fatal` or transport closure indicates session loss. There
are no host-bound frames between invocations and no reconnect/replay protocol.
Use `NewClient` rather than duplicating this state machine.

## Verified Runtime

Verified on 2026-09-10 on Linux amd64:

```text
denoland/deno@sha256:2014dc167ece617ef7e7ba40631ac2234c59e75ce693e7cc2dc2602b3c87859d
deno 2.9.6 (stable, release, x86_64-unknown-linux-gnu)
v8 15.0.245.2-rusty
typescript 6.0.3
```

The digest comes from `docker pull denoland/deno:2.9.6` and was confirmed with
`docker image inspect`. The release is
https://github.com/denoland/deno/releases/tag/v2.9.6, published 2026-08-27.
`RuntimeAssets` embeds the exact production recipe and runtime. `ProbeAssets`
embeds the standalone permission/loader diagnostic.

## Executed Tests

From the SDK module:

```sh
go test -race ./jsexec ./cmd/jsexecutor
JSEXEC_DENO_PROBE=1 JSEXEC_DOCKER_TEST=1 go test -race -count=1 -v ./jsexec
```

The Docker tests build the production image from embedded sources and execute
the same supervisor and runtime used by `NewDockerSession`. They cover retained
state, recoverable exceptions, ordered logs, eight concurrent callbacks,
fire-and-forget draining, immutable callback scope, malformed and premature
frames, raw stdio/default-channel forgery, prototype/bootstrap mutation,
privileged operations, worker permissions, background timer/worker termination,
native accounting tamper, bounds, cancellation, and allocation failure.

CPU-worker tests read actual `cpu.stat` and assert quota throttling before
cancellation. Memory tests verify cgroup configuration, force allocation
failure, and verify that the session cannot be reused; transport EOF does not
distinguish a V8 abort from a kernel OOM kill. No claim of that distinction is
made. Docker tests are opt-in and skipped by the default Go test invocation.
