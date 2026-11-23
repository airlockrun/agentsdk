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

`go tool air build` and `go tool air test` run deterministic generation/domain
tests without provisioning Docker or a database. `go tool air preview` uploads
the complete source candidate and fixtures and starts your personal real Airlock
app. General operations default to production; `--target preview` selects the
saved preview per app/remote. `air preview` never changes that default. `--remote` selects the
instance independently. `air run --local` is unsupported. Read
[remote development](reference/remote-development.md) for commands, resource
authority, fixture support and the current remote-suite boundary.

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
go tool air deploy status --wait --timeout 10m --logs
go tool air status --wait --timeout 10m
go tool air inspect
go tool air help preview
```

`deploy list [dir]` shows the newest builds with full build IDs, type, status,
start time, and message. `--limit` defaults to 10 and accepts 1-50.
`deploy status [dir]` inspects the latest build, or a specific `--build <UUID>`.
Both accept `--remote`, `--url`, and `--agent <slug-or-id>` with the same binding
conflict checks as source deployment, and use the saved login for that URL.
They do not upload source, start builds, or check SDK compatibility. When a
deployment accepted by this checkout finishes, status refreshes the canonical
source ETag in `.airlock/local/agent.toml` without changing source files. Login
refresh can update credentials outside the repository. `go tool air status`
shows runtime readiness; `go tool air inspect` exposes detailed deployment identities.

Status shows build lifecycle, deployment phase, timestamps, source ref, errors,
and job blockers. `--logs` adds persisted Docker and Sol logs. `--wait` polls
every two seconds, pins the selected build even if a newer build starts, and
prints only new log content; replaced or truncated snapshots are marked and
reprinted. `--timeout` defaults to 10m. Ctrl-C stops observation, not deployment.
A complete build is historical build state,
not a claim that its image is currently deployed or that the agent is running.

Public commands support `--json` using a versioned CLI result envelope; see
[the output contract](reference/cli-output.md). Build observations retain typed
API fields under `data`. Status JSON includes persisted logs even without `--logs`.
`--wait --json` emits one final result, not JSON Lines. Waiting for a failed build
exits nonzero with its snapshot and structured error. Snapshot reads succeed even
when the recorded build failed; consult its nested status. A building snapshot
without `--wait` succeeds immediately. An empty build history is an explicit
error. Subcommands immediately follow their parent; use `./list` or `./status`
for directories with those names. Unknown and inapplicable flags are rejected.

Direct resources use the same CLI locally and in hosted development:

```bash
go tool air resources list
go tool air resources inspect /personal_api
go tool air resources request /personal_api --path /v1/items
go tool air resources mcp-tools /personal_mcp
go tool air resources mcp-call /personal_mcp --tool search --data '{"query":"airlock"}'
go tool air resources list preview
go tool air resources bind preview/api --resource /personal_api
go tool air resources request production/api --path /v1/items
```

`request` calls an existing connection through Airlock's credential transport;
it does not create a resource or require an app binding. Local use resolves the
selected/default remote and user login. Hosted development uses its injected
execution credential on the admitted host, retaining live source restrictions
and resource grants without a separate login. No credentials are returned by
inventory or inspection. `resources bindings/bind/fixture/unbind` manage app
target selections separately. `/name` (or `/UUID`) selects a global resource;
`target/slot` selects an app binding. Bare selectors are rejected. Production
app-slot calls retain server approval gates. See [integrations](reference/integrations.md).

Airlock authorizes hosted production operations on the server. A blocked request
returns a durable challenge; the CLI reports it through the command supervisor's
machine channel and exits `approval_required` (77). The supervisor stops the shell
process group. Human decisions resume reasoning, never the original shell.
Production management and production execution use separate app/workspace/human
category grants for one hour after approval; use never renews that window. Matching
live category grants admit eligible subsequent requests without another challenge.
Narrow grants retain their exact scope and expiry. Every operation independently
checks live authority and source/configuration fences. Deployment captures an
immutable candidate and does not grant production-data execution access.
Personal previews and ordinary authenticated operator CLI commands retain their
normal service authorization.

Running `airlock` inside an agent repository delegates non-bootstrap commands
to `go tool air`; the repository's `go.mod` remains the version source of truth.

For package-level SDK details and runtime contracts, read the
[agentsdk reference](REFERENCE.md). Its focused companions cover
[object storage](reference/files.md), [interactive authentication](reference/auth-web.md), and
[Postgres-backed agents](reference/database.md). The [connector reference](reference/connectors.md)
covers hosted pure-Go connectors, typed contracts, local settings, and child transport.
Source migrations required by SDK releases are recorded in [UPGRADING.md](UPGRADING.md).
`agentsdk.UpgradeInstructions(fromVersion, toVersion)` returns the applicable
entries as Markdown in ascending version order.

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

Discover connectors alongside other authorized resources with `go tool air resources list`.
Use `resources inspect /NAME` (or `/UUID`) for readiness, host, artifact status and
capabilities. `resources list preview` lists app slots, not global inventory;
`resources inspect preview/SLOT` inspects a slot and `resources candidates preview/SLOT --type connector`
finds compatible bindings. Binding/use does not grant installation management.
`go tool air connectors inspect ID` diagnoses installation host/artifact/version
and management state; `hosts job`, `hosts request` and `hosts cancel` retain durable
management observation and recovery.

An authenticated operator with `manage` access to an enrolled host can run:

```sh
go tool air hosts exec HOST_UUID --url https://airlock.example --cwd /srv -- /usr/bin/id
go tool air connectors install --host HOST_UUID --agent APP_UUID --need sensor --artifact ARTIFACT_FILE_UUID --name Sensor
go tool air connectors update --host HOST_UUID --connector CONNECTOR_UUID --agent APP_UUID --need sensor --artifact ARTIFACT_FILE_UUID
```

Host targets are exact canonical UUIDs, not hostnames. Installation requires an
already enrolled host and an explicitly selected retained artifact file for the
app's declared connector need; it does not bootstrap an unregistered machine.
The host's local access mode must allow the operation. Ordinary CLI logins do not
use developer-session grants. Developer execution credentials remain subject to
the server's independently enforced execution ceiling.

Hosted developer install/update requires the existing `host_management_v1` human
confirmation for the exact host, workspace and human, valid for one hour. Exit 77
(`approval_required`) means no protected effect was executed: resolve the pending
confirmation in the developer conversation; do not replay the whole shell command.
An update changes the installed executable/settings, not just an app binding.
A multi-target need can bind a target group containing several installations;
each installation is reserved to one need.
The retained artifact must satisfy the installation's
management app/need and bound interfaces; binding does not transfer ownership.

Commands execute as the OS user running `airlock-host`, with executable and argv
passed separately (use an explicit shell executable for shell syntax). `--cwd`
sets the host working directory; omission uses the host process directory.
Repeat `--env KEY=VALUE` for environment overrides; other host environment values
are inherited. `--stdin-file PATH` supplies at most 1 MiB of input. Output has an
aggregate 512 KiB bound. `--timeout` bounds execution (default 5m; whole seconds
from 1s through 24h). Normal command output preserves stdout/stderr and nonzero
exit status; process/transport errors are failures, not successful empty output.

Commands wait by default. Install/update additionally wait for ready inventory
with the exact artifact digest and host identity before reporting verification.
The job's own inventory revision and acknowledgment are required, including for
same-executable settings updates; an older ready inventory is insufficient.
`--settings-file PATH` supplies a JSON settings object; update omission retains
the existing settings. Progress goes to stderr. `--json` emits the generated job
response on stdout; terminal failures still return a nonzero status.
`--no-wait` returns the durable job identity without claiming installed/updated
state. `go tool air hosts job JOB_UUID` inspects a management job (`--wait` waits
for its terminal status; connector inventory verification belongs to install/update).
Interrupting observation does not cancel a remote job; its server deadline and
host lease rules remain active. Observation failures never resubmit commands or
connector lifecycle operations automatically.

`go tool air hosts cancel JOB_UUID` requests fenced cancellation (`--wait` observes
settlement). This does not undo effects or assert that a remote process has already
stopped. Hosts must report management protocol version 2 to accept management work;
the WSS transport remains `airlock.host.v2` and connector child protocol remains V1.
Developer execution credentials additionally require one explicit, nonrenewing
one-hour host-management approval for the exact host, developer workspace and
verified originating human. That one grant covers commands, installation and
update; it confers no missing host permissions and does not apply to other hosts.
Ordinary CLI logins have no developer-session grant requirement.

Every command/install/update admission sends a UUID request identity. The CLI
prints it before submitting. Preserve `--request-id UUID` and the exact arguments
after a timeout or lost response; repeating that identity/body returns the original
durable job, not another execution. A changed payload/host with the same caller's
identity is a conflict. `go tool air hosts request HOST_UUID REQUEST_UUID --json`
recovers a committed admission without needing settings or stdin again. A 404
means not observed, not permission to submit another UUID: an in-flight admission
can still commit. Host history includes unresolved work beyond its recent window;
completed-but-unacknowledged lifecycle work remains observable. Correlation records
are retained for durable idempotency rather than removed by ordinary job cleanup.

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

Local tests exercise domain logic, direct handlers and offline manifests using
explicit typed fixtures and `goai/testutil` model mocks. `agentsdk/testutil`
injects validated caller metadata without authentication or runtime work.
Real startup, migrations, SDK HTTP admission, tasks/jobs and Deno integration
execute on Airlock targets. `agentsdk/remote` supplies an explicit operator client
for HTTP, registered tools, durable agent lifecycle and server-side bindings.
`air test --remote NAME --target preview --revision UUID --suite integration --wait`
requests the candidate's packaged real-host suite. `remote.TestClientFromEnvironment`
uses a scoped opaque runner capability, never laptop login/provider credentials
or a simulated host. Source contains the entire fixture set.
See [remote development](reference/remote-development.md).

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
