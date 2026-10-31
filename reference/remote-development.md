# Remote app development

The employee workflow needs Go and the pinned generation tools, not Docker.
`go tool air build` generates SQLC/templ/Tailwind output, runs deterministic local
tests and compiles the app/connectors. `go tool air test` runs local unit tests.
Neither command provisions a database or host. Default app source/test imports
of `localruntime`, `agenttest` or testcontainers fail explicitly. Direct domain
tests use typed fixtures, `goai/testutil` model mocks and `agentsdk/testutil`
caller context. `air run --local` is unsupported.

## Select the instance and deployment independently

`.airlock/local/agent.toml` contains `default_remote` and named `[remotes.NAME]`
entries. Each entry binds an Airlock origin and a default logical app ID.
`--remote NAME` selects the instance without changing `default_remote`; explicit
`--agent` selects another app on the remote without rebinding its default.

General app operations default to `production` when `--target` is omitted.
`--target preview` selects the saved preview for the selected remote and logical
app; a missing reference is an error. Named targets, including `staging`, work
when configured on the server. Source metadata and pending tasks are cached per
concrete runtime ID so deployments do not share ETags. Config schema is `targets-v1`.

First `air preview` creates an independent creator-associated deployment and saves
its concrete ID/name under `[remotes.NAME.previews.APP_UUID]`. Subsequent calls update
that deployment. `air preview` never changes the default for later commands.
Laptop previews have no owning workspace. The CLI stores no checkout identity or
absolute path. Fresh checkouts without a selected reference create independent
previews. Copied or moved configuration may retain the same reference and source
cache; server authentication and authorization decide access. Explicit selection uses
`air preview select <concrete-target-name-or-UUID> --dir <checkout>`. Server access
checks decide whether selection is allowed; selecting never transfers ownership.
Normal preview creation/resume needs no manually chosen target name.

```sh
go tool air login https://airlock.example
go tool air init my-app --url https://airlock.example
go tool air preview my-app --create --name my-app -m "Initial app" --wait
go tool air preview my-app -m "Try new report" --wait
go tool air status my-app --target preview --json
go tool air deploy list my-app --target preview
go tool air deploy status my-app --target preview --watch --logs
```

`air preview` creates or updates the saved preview. `air deploy --target preview`
requires a saved selection. Both upload the entire canonical source bundle (including tests/testdata and
`preview-fixtures.json`), records the durable deployment task, and optionally waits
for actual running-state confirmation. URLs come from server `route_base_url`
metadata (`appslug--random-deploymentslug.host`), not usernames or client string
construction. Preview app slugs must fit 27 bytes for the host's random
`p-<32 lowercase hex>` target name and the 63-byte DNS label limit. The host
rejects longer slugs without truncation. It does not regenerate managed source files,
fetch individual fixture files from the laptop, run a model or build a local image.
Use `air update` explicitly to reconcile SDK-managed files. `.git`, `.airlock`,
caches, ignored files and special files do not enter source archives. The archive
digest is computed from the exact streamed archive bytes.

`air pull --target preview` uses the saved selection and target-scoped CAS.
Cloning `--target preview` requires an existing reference for that app/remote; use a
concrete target name to clone source outside it. A clone downloads source, not
ownership/preview association; use explicit `preview select` to share intentionally.
A source conflict requires pull/merge or deliberate
`--force`; credentials remain outside the source tree. `--wait`/Ctrl-C stops
observation, not the server deployment task. Build status and runtime status are
separate: inspect `air deploy status` for a particular build, `air status` for
the selected deployed runtime.

## Inspect builds without uploading source

`air deploy list [dir] --target preview --limit 10` returns newest-first build
IDs (`--limit` accepts 1–50). `air deploy status [dir] --target preview` selects
the latest build; `--build UUID` pins another. Both accept `--remote`, `--url`
and `--agent`. Use `./list` or `./status` for directory names matching subcommands.

Status `--watch --logs` pins the selected build, polls every two seconds and
prints persisted Docker/Sol log additions until complete, failed or interrupted.
Replaced/truncated log snapshots are marked and reprinted. Build phase describes
that build, not the currently deployed image. These observation commands do not
upload source or require SDK compatibility; they use saved operator credentials
(refresh can update the external login store), not codegen integration tokens.

`--json` emits typed API response fields, including `routeBaseUrl` and
`workspaceUrl`; status includes persisted logs. It cannot combine with `--watch`.
Failed builds emit the snapshot before returning a nonzero exit and stderr error,
including in JSON mode. An unfinished snapshot without watch succeeds immediately;
no builds is an error. Status JSON's `deployment.routeBaseUrl` and build JSON's
`routeBaseUrl` are exact human-navigation URLs from the selected target. Share
them unchanged; `workspaceUrl` is a separate dashboard conversation link and can
be empty for laptop previews. Verify through authenticated app/tool operations,
not an unauthenticated GET or a token in a human link.

## Operate the real app

```sh
go tool air request GET / --target preview
go tool air tools list --target preview
go tool air tools invoke report --input '{"period":"week"}' --target preview
go tool air agent start summarize --input '{"path":"/input.csv"}' --target preview
go tool air agent get summarize <run-id> --target preview
go tool air agent list summarize --target preview
go tool air agent wait summarize <run-id> --target preview --timeout 120s
go tool air agent cancel summarize <run-id> --target preview
go tool air agent continue summarize <session-id> --prompt 'Summarize the next input' --target preview
go tool air logs <run-id> --target preview
go tool air stop --target preview
go tool air start --target preview --wait
```

Stop retains target DB/files/bindings; it is not deletion. Deleting a laptop checkout
emits no server event. Host idle TTL/admin policy handles preview cleanup.
Abandoned laptop previews default to seven-day idle retention under host policy;
the CLI neither claims deletion nor prevents expiry by having a local config.

HTTP requests use origin-relative paths, retain query/body and return redirects
without following them. Tool calls use registered handlers without a chat loop.
Agent starts and continuations print their request ID before admission; supply
`--request-id UUID` to retry an uncertain admission safely. Wait cancels observation
only; cancel is an explicit durable-controller operation. Wait prints the terminal
run and returns a nonzero exit for failed/cancelled work. Hosted execution lists
and controls only tasks admitted from its workspace. Logs are a plain-text snapshot;
log following is not supported.

The public `agentsdk/remote` client offers explicit Config URL/AppID/Target/Revision,
token callback and HTTP client; Get/CreatePreview/SetPolicy/Lifecycle, Request,
InvokeTool, Start/Get/List/Wait/Cancel/ContinueAgent and binding operations. Revision
checks validate the observed deployment; HTTP/tool/agent admission requires a
nonempty revision.
The CLI uses the observed target revision unless `--revision` explicitly pins it.
The client sends `X-Airlock-Expected-Source-Revision`; host admission applies its
revision ceiling before execution. Scoped suite callbacks are also bound to the
server record's source/runtime/config generation.
The public client defaults to production and requires a concrete name/ID for
preview operations. CreatePreview returns a new deployment; select its returned
name/ID in a new client. CLI operations resolve the saved reference through the
server and pin its concrete target. Inside a real Airlock-hosted development
workspace, commands default to the injected preview. Host injection includes
`AIRLOCK_APP_ID`, `AIRLOCK_AGENT_ID`, `AIRLOCK_DEPLOYMENT_TARGET` and
`AIRLOCK_DEVELOPER_SESSION_ID`. Explicit production/named targets reach existing
host authorization; conflicting app/remote/origin coordinates are rejected.
The client receives user credentials, never native app tokens or caller assertions.

`go tool air preview -m "Describe changes" --launch` also starts an existing
stopped/suspended image. An unchanged-source response completes without a new
build task. Hosted UI launch queues this same fixed CLI workflow after the active
turn's final checkpoint, using a durable workspace command. Later submitted chat
messages remain visible and wait for a ready, failed or resource-setup-blocked
preview outcome. The model receives the outcome as a header or host notification.
Hosted command credentials include exact task ownership for source admission;
the host supplies the observed source ETag for a restored UI launch checkout.
No shell effects are automatically replayed after interruption. The developer
can inspect the authenticated preview with `air request` and registered tools;
browser automation is not a hosted developer capability.

## Resources: direct user use and target binding

```sh
go tool air resources list --remote corp
go tool air resources inspect /personal_api --remote corp
go tool air resources request /personal_api --path /v1/items --remote corp
go tool air resources mcp-tools /personal_mcp --remote corp
go tool air resources mcp-call /personal_mcp --tool list --data '{}' --remote corp
go tool air resources bindings --target preview
go tool air resources bind preview/crm --resource /personal_api
go tool air resources fixture preview/crm --fixture canned-crm --revision <candidate-revision>
go tool air resources unbind preview/crm
```

Direct-resource commands select only user+remote; an app/target is neither needed
nor accepted. Hosted development uses the injected execution credential and
admitted host without another login. Direct calls use your live resource grants
and source conversation/bridge/MCP restrictions, independently of app membership
or bindings. An explicitly selected hosted remote must match that host.
`resources list` discovers global resource slugs and stable UUIDs. Use `/slug` or
`/UUID` for global resources, and `preview/slot` or `production/slot` for app
bindings. Bare selectors are rejected. `resources list <target>` discovers slots;
ambiguous slot types require `--type`. A target in an address must agree with
`--target`. Named targets must exist in the backend. App-bound production calls
retain production execution approval. Live production binding edits remain
protected configuration: prepare their selections with `deploy --bind` in the
immutable candidate and follow its deployment approval. Target binding separately checks app/target rights and resource
`use`, without an assign capability. All bindings are stored on the server; no
local binding file or secret argument is used. `air preview --bind [type:]slot=UUID`
and `--fixture slot=name` attach `X-Airlock-Candidate-Config` to source admission.
The host stores that plan with the candidate, validates inspected declarations,
compatibility/resource use and fixture snapshots, then commits it at cutover.
There is no pre-build live binding mutation or binding PUT race against OnStart.

For a setup-blocked candidate, use `air preview configuration <candidate-UUID>`
to inspect its manifest/selections, then add `--bind [type:]slot=UUID` and/or
`--fixture slot=name` to update pending selections. This uses the candidate
configuration API and does not alter the currently running target's bindings.
The host validates and resumes the pending build; observe its deployment task
with `air status --target preview --wait`.

Static HTTP fixtures use `preview-fixtures.json` with version 1 and named fixtures
bound to slots. Bodies can be inline or `bodyFile` paths inside the whole admitted
source. The host validates and snapshots fixtures from the exact candidate revision.
Misses fail; they never call a real endpoint. Use the atomic candidate selections
on preview creation/update or deliberate edits to current target bindings. HTTP fixtures
are preview/shared only; MCP/model/stateful fixture emulation is not advertised.

## Integration suites

Keep real-host integration Go packages behind `//go:build airlockintegration`,
with test fixtures in the whole candidate source bundle. Remote suites are
explicit opt-in; local test/build commands run deterministic domain checks
without Docker and do not run remote suites.
Run `air test --remote NAME --target preview --revision UUID --suite
integration --wait` to admit the exact deployed candidate. The host builds one
test binary per package from the exact candidate, with packaged working-directory
fixtures, and executes them in a separate suite runner. Missing capability must
not become a successful skipped test.

Tests execute against the selected preview's existing app runtime, database,
files and resource bindings. Airlock does not create a fresh app runtime or
clone/reset state for each test or suite. Tests and interactive preview use
share state, so test mutations remain visible there. The preview's app state is
isolated from production; bound real resources retain their own external state.
Test binaries use
`remote.TestClientFromEnvironment()` with a scoped opaque runner capability and
exact app/deployment/source/runtime coordinates, not laptop login/provider/DB
credentials. Its API is `Request`, `InvokeTool` and
`StartAgent`/`GetAgent`/`WaitAgent`/`CancelAgent`, with no login, binding, deployment
or resource-management operations. Runner calls are checked against the pinned
source revision, runtime generation and configuration revision.
`air test status|cancel --run UUID` uses the same remote/target/revision;
closing observation does not cancel server work. Missing API capability fails.
Suite execution requires host runner packaging and admission support. SDK
HTTP-contract tests/compilation do not certify real packaging or lease/cancellation
behavior. Use actual host verification results before claiming live suite readiness.

Platform developers may opt into a full actual Airlock dev stack using host
tooling. It is separate from employee SDK defaults. The simulator source/APIs and
local app launcher are physically removed, with no wrappers or data conversion.
