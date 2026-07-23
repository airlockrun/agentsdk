# agentsdk

Go SDK for building **cyborg agents** — programs that are half code, half AI — that run on [airlock](https://github.com/airlockrun/airlock).

Cyborg agents are deterministic Go where it makes sense (HTTP routes, webhooks, cron jobs, structured tool execution) and AI-driven where it helps (LLM reasoning, conversation handling, open-ended decisions). agentsdk is the contract your code uses to participate in the airlock platform: register routes, tools, webhooks, crons, and chat surfaces; access scoped storage and per-agent Postgres; and call LLMs through the platform's credential-managing proxy.

If you're not building on airlock, you don't need this — agentsdk is the glue, not the runtime.

> [!WARNING]
> **Alpha software.** Early-stage code with bugs we haven't found yet — please [open an issue](https://github.com/airlockrun/agentsdk/issues) for anything that breaks. The current RC line is pre-public and may include coordinated breaking migrations. Internal/unexported code can change freely. See [Stability](#stability) below.

## Install

```bash
go get github.com/airlockrun/agentsdk
```

Requires Go 1.26+.

## Air CLI

Install the global launcher once:

```bash
go install github.com/airlockrun/agentsdk/cmd/airlock@v0.4.0-rc.32
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

For the complete SDK surface and runtime contracts, read the
[agentsdk API reference](REFERENCE.md). Its focused companions cover
[object storage](reference/files.md), [remote execution](reference/exec.md),
[interactive authentication](reference/auth-web.md), and
[Postgres-backed agents](reference/database.md).

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

In a real agent you'd also call `RegisterTool`, `RegisterWebhook`, `RegisterCron`, `RegisterConnection`, and so on. The [API reference](REFERENCE.md) documents the full surface.

## Stability

The current `v0.4.0-rc.N` line permits coordinated breaking migrations with an RC increment and explicit migration instructions. Published stable releases preserve their author-facing API compatibility.

Internal/unexported code can change freely. Non-trivial API changes go through a Discussion before any PR — see [CONTRIBUTING.md](CONTRIBUTING.md).

The root package contains only APIs used by agent code. Airlock HTTP payloads
and runtime bookkeeping are package-private. Test code uses
`github.com/airlockrun/agentsdk/agenttest` for its mock Airlock and environment.

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
