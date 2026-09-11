# agentsdk

Go SDK for building **cyborg agents** — programs that are half code, half AI — that run on [Airlock](https://airlock.run).

Cyborg agents are deterministic Go where it makes sense (HTTP routes, webhooks, cron jobs, structured tool execution) and AI-driven where it helps (LLM reasoning, conversation handling, open-ended decisions). agentsdk is the contract your code uses to participate in the airlock platform: register routes, tools, webhooks, crons, and chat surfaces; access scoped storage and per-agent Postgres; and call LLMs through the platform's credential-managing proxy.

The root SDK integrates Go apps with Airlock. The public
[`chatruntime`](chatruntime/README.md) package runs hosted chat independently of
the SDK root, using explicit Sol models, session persistence, capability dispatch,
and an isolated Deno executor.

Read the [Airlock documentation](https://airlock.run/docs/) for platform guides and the [Agent SDK and CLI guide](https://airlock.run/docs/agentsdk/) for the authoring workflow.

## Install

```bash
go get github.com/airlockrun/agentsdk
```

Requires Go 1.26+.

## Air CLI

Install the global launcher once:

```bash
go install github.com/airlockrun/agentsdk/cmd/airlock@latest
```

The launcher selects the Agent SDK version advertised by the target Airlock and
creates a repository that pins its own CLI:

```bash
airlock init my-app --url https://airlock.example.com
airlock clone existing-app --url https://airlock.example.com my-app
```

Inside an agent repository, use the pinned tool for authoring and deployment:

```bash
go tool air toolchain install
go tool air build
go tool air deploy -m "Describe this deployment"
go tool air deploy list --limit 10
go tool air deploy status --watch --logs
```

`deploy list [dir]` shows the newest builds with full build IDs, type, status,
start time, and message. `--limit` defaults to 10 and accepts 1-50.
`deploy status [dir]` inspects the latest build, or a specific `--build <UUID>`.
Both accept `--remote`, `--url`, and `--agent <slug-or-id>` with the same binding
conflict checks as source deployment, and use the saved login for that URL.
They do not upload source, start builds, check SDK compatibility, or write the
workspace. Login refresh can update credentials outside the repository.

Status shows build lifecycle, deployment phase, timestamps, source ref, errors,
and job blockers. `--logs` adds persisted Docker and Sol logs. `--watch` polls
every two seconds, pins the selected build even if a newer build starts, and
prints only new log content; replaced or truncated snapshots are marked and
reprinted. Ctrl-C cancels the watch. A complete build is historical build state,
not a claim that its image is currently deployed or that the agent is running.

Both commands support `--json` using the typed API response (`builds` for list,
`build` for status). Status JSON includes persisted logs even without `--logs`.
`--watch --json` is rejected. Failed status prints its snapshot, then exits
nonzero with an error on stderr, including in JSON mode. A building snapshot
without `--watch` succeeds immediately. An empty build history is an explicit
error. A valid `deploy list -m "message"` uploads a directory named `list`;
use `./list` or `./status` to make these directory names unambiguous.

Running `airlock` inside an agent repository delegates non-bootstrap commands
to `go tool air`; the repository's `go.mod` remains the version source of truth.

For package-level SDK details and runtime contracts, read the
[agentsdk reference](REFERENCE.md). Its focused companions cover
[object storage](reference/files.md), [interactive authentication](reference/auth-web.md), and
[Postgres-backed agents](reference/database.md). The [connector reference](reference/connectors.md)
covers hosted pure-Go connectors, typed contracts, local settings, and child transport.

## Hello-world agent

```go
package main

import (
	"fmt"
	"net/http"

	"github.com/airlockrun/agentsdk"
)

func main() {
	agent := agentsdk.New(agentsdk.Config{
		Description: "Greets visitors. Replace once the agent does real work.",
	})

	agent.RegisterRoute(&agentsdk.Route{
		Method: http.MethodGet,
		Path:   "/",
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			_, err := fmt.Fprintln(w, "hello from a cyborg agent")
			return err
		},
		Access:      agentsdk.AccessPublic,
		Description: "Greet anyone who visits the home route.",
	})

	agent.Serve()
}
```

In a real agent you'd also call `RegisterTool`, `RegisterWebhook`, `RegisterJob`, `RegisterConnection`, and so on. The [API reference](REFERENCE.md) documents the full surface.

Agent repositories may also contain connector binaries under immediate
`connectors/<slug>` directories. Shared contracts use
`github.com/airlockrun/agentsdk/connector`; `go tool air build` validates each
native manifest and cross-compiles its explicitly declared Linux, macOS, and
Windows targets with `CGO_ENABLED=0`.

Connector artifacts are installed and supervised by `airlock-host`, which
verifies the exact size and SHA-256 before executing a candidate. Connector
binaries use framed stdin/stdout child transport and never hold an Airlock
credential.

`go tool air connectors list` and `go tool air connectors inspect <id>` inspect
visible installed connector resources without executing operations.

## Lifecycle

`agentsdk.New` creates a definition-only agent. Construct services, wire the
late-bound handles returned by APIs such as `Agent.DB()`, and register every
declaration in your factory. This phase does not read runtime environment
variables, open the database, make network calls, or run migrations. Database
operations through the `Agent.DB()` handle are unavailable until startup.

`Agent.Manifest()` freezes declarations and returns the complete canonical
manifest without starting runtime dependencies. Running the agent with
`AIRLOCK_AGENT_MODE=manifest` emits that manifest as one JSON line and exits, so
Airlock can inspect an agent offline.

In normal mode, `Agent.Serve()` freezes declarations, starts the runtime and
runs migrations, synchronizes the manifest with Airlock, runs named
process-local `OnStart` hooks, then serves HTTP until shutdown. Keep durable
startup work in registered jobs; `OnStart` is for disposable process-local
state.

Tests use `agenttest.New(t, factory)`. It invokes the factory first while the
agent is definition-only, then provisions the mock Airlock and test database,
starts the runtime, validates migrations with an up, down-to-zero, up cycle,
synchronizes declarations, runs `OnStart` hooks, and returns a ready agent.
`go tool air build` provisions one throwaway PostgreSQL container for the serial
test run instead of starting one per `agenttest.New` call.

`env.Chat(ctx, scope, input)` exercises the actual shared Sol chat loop with mock
models and the real authenticated app capability handler. Use `agenttest.MemoryStore`
for conversation history, `agenttest.Events` for typed events, and
`agenttest.Executor(ExecutorConfig)` for a lazy Deno factory. Local tests select
an executor image built with `jsexec.BuildImage`; builders inject `OpenTransport`
to an isolated host-owned executor without mounting Docker into test containers.
Text-only and unapproved runs do not allocate an executor. See the
[chat runtime contract](chatruntime/README.md) for required settings and callbacks.

## Companion projects

- [airlock](https://github.com/airlockrun/airlock) (AGPL-3.0) — the self-hosted platform that runs agents built with this SDK
- [goai](https://github.com/airlockrun/goai) (Apache-2.0) — Go port of the Vercel AI SDK
- [sol](https://github.com/airlockrun/sol) (Apache-2.0) — agent runtime / CLI utility

## License

[Apache-2.0](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). A CLA Assistant bot will prompt you to sign on your first PR (one signature covers all airlockrun projects).

## Security

Email `security@airlock.run`. Do not open public issues for vulnerabilities.
