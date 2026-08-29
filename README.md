# agentsdk

Go SDK for building **cyborg agents** — programs that are half code, half AI — that run on [Airlock](https://airlock.run).

Cyborg agents are deterministic Go where it makes sense (HTTP routes, webhooks, cron jobs, structured tool execution) and AI-driven where it helps (LLM reasoning, conversation handling, open-ended decisions). agentsdk is the contract your code uses to participate in the airlock platform: register routes, tools, webhooks, crons, and chat surfaces; access scoped storage and per-agent Postgres; and call LLMs through the platform's credential-managing proxy.

If you're not building on Airlock, you don't need this — agentsdk is the glue, not the runtime.

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
```

Running `airlock` inside an agent repository delegates non-bootstrap commands
to `go tool air`; the repository's `go.mod` remains the version source of truth.

For package-level SDK details and runtime contracts, read the
[agentsdk reference](REFERENCE.md). Its focused companions cover
[object storage](reference/files.md), [interactive authentication](reference/auth-web.md), and
[Postgres-backed agents](reference/database.md). The [connector reference](reference/connectors.md)
covers pure-Go remote-machine connectors, typed contracts, local settings, and service lifecycle.

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

macOS connector artifacts are unsigned and not notarized. Verify the published
SHA-256 before installation; if Gatekeeper quarantines the verified file, remove
only that file's quarantine attribute with `xattr -d com.apple.quarantine
/path/to/connector`. The [connector reference](reference/connectors.md) details
the user LaunchAgent installation path and desktop privacy (TCC) limits. macOS
system services are unsupported.

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
