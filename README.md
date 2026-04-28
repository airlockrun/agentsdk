# agentsdk

Go SDK for building **cyborg agents** — programs that are half code, half AI — that run on [airlock](https://github.com/airlockrun/airlock).

Cyborg agents are deterministic Go where it makes sense (HTTP routes, webhooks, cron jobs, structured tool execution) and AI-driven where it helps (LLM reasoning, conversation handling, open-ended decisions). agentsdk is the contract your code uses to participate in the airlock platform: register routes, tools, webhooks, crons, and chat surfaces; access scoped storage and per-agent Postgres; and call LLMs through the platform's credential-managing proxy.

If you're not building on airlock, you don't need this — agentsdk is the glue, not the runtime.

> [!WARNING]
> **Alpha software.** agentsdk is in 0.x and breaking changes are still possible — but we treat them as a last resort, not a default. The public API is intended to stay relatively stable and backwards-compatible even pre-1.0, because **agents built against an older agentsdk version need to keep working against newer ones** at the airlock platform level. Internal/unexported code can change freely. See [the Stability section below](#stability).

## Install

```bash
go get github.com/airlockrun/agentsdk
```

Requires Go 1.26+.

## Hello-world agent

```go
package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/airlockrun/agentsdk"
)

func main() {
	agent := agentsdk.New(agentsdk.Config{
		Description: "Greets visitors. Replace once the agent does real work.",
	})

	agent.RegisterRoute("/", "GET", func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello from a cyborg agent")
	}, agentsdk.RouteOpts{
		Access:      agentsdk.AccessPublic,
		Description: "Greet anyone who hits the agent's home route.",
	})

	agent.Serve()
}
```

In a real agent you'd also call `RegisterTool`, `RegisterWebhook`, `RegisterCron`, `RegisterConnection`, and so on — see the airlock docs for the full surface.

## Stability

agentsdk's public API is treated as a **stability commitment**: changes to exported types, functions, or runtime behavior are kept backwards-compatible across minor versions. Older built agents must continue to work against newer agentsdk releases.

Internal/unexported code can change freely. Non-trivial API changes go through a Discussion before any PR — see [CONTRIBUTING.md](CONTRIBUTING.md).

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
