# Agent SDK upgrade instructions

This file contains consolidated source migrations for SDK target releases.
Airlock selects entries whose versions are greater than the application's source
SDK version and at most the target SDK version. The v0.8.1 entry describes the
complete final migration from earlier stable and prerelease SDKs. Apply only the
instructions relevant to the application's source. An application already on
v0.8.1 receives no instructions when rebuilding against v0.8.1.

## v0.8.1

Environment handles read an immutable process-start configuration snapshot.
Remove calls to `EnvVarHandle.Refresh()`; save configuration through Airlock and
explicitly restart the target to apply it. Rebuild apps and the host together:
manifests require `envConfigProtocol: airlock.env-config.v1` under
`airlock.app-runtime.v4`. `Agent.Start` requires the host-provided snapshot for
declared environment slots before startup hooks run. See
[`reference/environment.md`](reference/environment.md).

Use the unified `go tool air resources` CLI in scripts and developer instructions.
Global resource addresses are `/name` or `/UUID`; app bindings use `target/slot`.
Use `resources list [target]`, `resources inspect ADDRESS`, `resources request
ADDRESS --path /path`, `resources mcp-tools ADDRESS`, `resources mcp-call ADDRESS
--tool NAME --data JSON`, and `resources mcp-probe URL`. Remove invocations of
the top-level `integrations`, `connection`, and `mcp` command families. Bind with
`resources bind preview/slot --resource /name`, adding `--type` for ambiguous
slot types. Bare resource selectors are not accepted. Production app-slot calls
retain host approval gates; production configuration uses the approved immutable
candidate rather than a production execution grant.

Generic SQL is not a runtime capability. Remove `agentsdk.CapabilityQueryDB`
from capability policies. Database work belongs in explicit typed app-owned Go
tools using `Agent.DB()`; task agents can declare those private tools. Runtime
scripts use those tools rather than `air.queryDB` or `apps.<alias>.db.query`.

App identity uses `agentsdk.Config.Icon` with one domain-specific emoji, for
example `Icon: "🎧"`. Update `Config` literals that specify `Emoji` to use
`Icon`, retaining the chosen value. Canonical SDK manifests use JSON `icon`.
Image URLs and icon-library names are not supported. Empty icon declarations
preserve the stored app icon. SDK and host must be rebuilt together.

Remove `Tool` fields from `agentsdk.Route` declarations. Share application logic
explicitly between HTTP handlers and registered tools; their access policies are
independent. Set `Logging: agentsdk.LoggingNormal` on sensitive GET/HEAD reads and
exports (corporate secrets, personal data, privileged downloads). Method-default
logging is Off for GET/HEAD and Normal for other methods. Polling and static
assets use Off. Do not implement mutations through GET.

Applications require `airlock.app-runtime.v4` and a matching host rebuild.
Route run-required SDK APIs preserve the host-admitted human or anonymous caller.
Caller reads perform no I/O. Handlers must honor the absolute 30-minute ingress
deadline; durable work belongs in registered jobs. Route metadata never implies
permission to invoke a tool. See `reference/html-ui.md`.

### Long-running tools and application tests

Sol provider attempts have a 15-minute wall ceiling and five-minute meaningful
progress inactivity ceiling. Each tool has a fresh ten-minute ceiling, subject
to shorter operation-specific, caller and task deadlines. Convert long tools to
`RegisterAsyncTool` with an explicit typed `Job` policy. Return the accepted
`AsyncTask` promptly; observe completion with host task controls/subscriptions.
Synchronous handlers must honor cancellation. Cancellation does not roll back
effects; inspect unknown outcomes before retrying. See `reference/jobs.md`.

Application tests use deterministic domain/handler/model fixtures locally and
real Airlock previews for integration. Remove imports of the SDK `localruntime`
and root `agenttest` packages. The SDK exports no local host, Env/New/NewWithOptions,
RunAgent, persistent FileStorage, local model-account selectors or local app
launcher. There are no compatibility aliases or local-state data converters.

Direct caller fixtures use `agentsdk/testutil.WithCaller(ctx, wire.Caller{...})`;
models use explicitly injected `goai/testutil.MockModel`. Offline composition
checks `newAgent().Manifest()`. Put runtime/migration/storage/agent tests behind
`//go:build airlockintegration`; `remote.TestClientFromEnvironment()` uses the
real runner's scoped opaque capability and exact source/runtime coordinates.
Do not copy laptop login/provider/DB credentials into a test runner.

Use `goai/testutil.NewMockModel(testutil.MockConfig{ID: ..., ...})` for streaming
model tests. Configure Text, ToolCalls and Usage in MockResponse, or set Events
for a precise event sequence. Responses supplies a sequence, Default answers
unmatched requests, and Rules selects responses by LastUserText or Match.
Handle the constructor error. Read captured call options through Requests();
Configure changes response behavior while retaining the model's ID.
MockConfig.Stream supplies custom request-boundary streaming behavior. Inject
the model directly into domain services; live evaluations use explicit remote
suites with host-authorized models and budgets. See
`/libs/agentsdk/reference/testing.md`.

`air preview` creates or updates the saved preview per app/remote. General app
commands default to production; `--target preview` requires a saved reference.
Inside a real hosted development workspace, the injected preview is the default.
Candidate `--bind [type:]slot=UUID` and `--fixture slot=name` are atomic source
admission plans, applied by the host at cutover, not live-binding PUTs.
`air test --remote NAME --target preview --revision UUID --suite integration
--wait` executes that candidate's packaged real-host suite. All fixture files are
in the whole source candidate. Platform PG/Deno/image verification is explicitly
selected with `airlockplatformintegration`, outside default SDK unit testing.

### Task submissions and review

Use the model-facing `submit` control in task instructions and mock tool calls.
Its output branch is `{kind:"output",output:<typed result>,artifacts:[{path:"..."}]}`;
artifacts are optional and may include expected `sha256` or `contentId` assertions.
Its question branch is `{kind:"needs_input",question:"..."}`. Inspect reply kind
after completed status; a submitted output is a draft until accepted.

Optional `AgentDefinition.Review` declares a typed `AgentReview[In, Out]` with
positive Version, Timeout (whole milliseconds), MaxAttempts, MaxConcurrency,
nonnegative MaxCorrections and Handler. Return `Accept()`, `Fail(reason)` or
`Continue(feedback)`; nonempty feedback is bounded to 4096 bytes. Use the supplied
`AgentReviewContext.Open(ctx, artifact)` for pinned artifact bytes and the matching
callback context for `handle.Input(ctx)`. Deduplicate reviewer external effects by
submission ID because delivery is at least once.

Handle `AgentRunSubmitted` and `AgentRunReviewing` as nonterminal. Automatic
correction retains the same run ID and cumulative reasoning budget. Public
Continue addresses a completed session and starts a new run. Reviewed
definition/submission integration uses the real host controller; pure engine,
checkpoint and review validation uses deterministic runtime unit tests.

Hosts must support checkpoint version 2 and atomically commit submission,
artifact pins, transcript and bound review scheduling in SaveCheckpoint.
Controller.Complete performs validation only. Result.Submitted marks Reply as a
draft. Persisted version 1 completed journals retain their recorded replies;
in-flight version 1 journals require draining or explicit termination before
cutover. Deploy compatible SDK/host binaries together and retain exact admitted
executable identity throughout review/correction; declaration hashes alone do
not identify callback code. See `/libs/agentsdk/reference/agents.md`.

### Storage declarations

Declare every app-storage prefix used by native Go code with
`RegisterDirectory`. Use `AccessInternal` for private native-only paths, and
preserve the appropriate human access policies and `Scope` for paths exposed to
users or untrusted callers. Native methods such as `WriteFile`, `OpenFile`,
`ListDir`, `CopyFile`, `ShareFileURL`, `SyncUp`, and `SyncDown` reject paths that
do not belong to a registered directory.

Do not register a broad synthetic prefix to cover unrelated paths. Declare the
actual storage roots so the catalog, Files visibility, and longest-prefix nested
directory policy remain consistent.

### HTMX 4

Update applications that use htmx from htmx 2 to the bundled htmx 4 core asset.
Applications without htmx usage require no source changes, but still verify the
application with the normal generated-code, test, and build checks.

Read `/libs/agentsdk/reference/html-ui.md`, then read the version-matched
`.airlock/toolchain/skills/htmx/SKILL.md` and
`.airlock/toolchain/skills/htmx/reference/htmx-upgrade-from-htmx2.md` before
editing. Inspect all `hx-*` and `data-hx-*` attributes, htmx event listeners,
JavaScript API calls, and server-side `HX-*` header handling. Use the bundled
`agentsdk.Assets.HTMX` URL rather than a htmx 2 asset or an extension bundle.

Apply the attribute renames in this order: first replace htmx 2 `hx-disable`
with `hx-ignore`, then replace `hx-disabled-elt` with htmx 4 `hx-disable`.
Add explicit `:inherited` modifiers only where descendants rely on a parent's
attribute. Convert event names to colon-separated htmx 4 names and update event
handlers to use request and response state under `event.detail.ctx`. Review
error responses because htmx 4 swaps HTTP error bodies by default; use
`hx-status`, `hx-swap="none"`, or `htmx.config.noSwap` where an error must not
replace its target.

Preserve the generated authentication behavior: 401 remains a no-swap status,
and the `htmx:response:error` handler reads
`event.detail.ctx.response.status` and reloads the page so Airlock can enter the
authentication relay. Preserve application routes, styling, data contracts,
and unrelated behavior.
