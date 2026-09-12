# Runtime Ingress

`Agent.Handler()` is a private host-to-app listener. Every request except the
built-in `GET`/`HEAD /health` probe requires exactly one
`Authorization: Bearer <AIRLOCK_AGENT_TOKEN>` header. This includes custom routes
(even `AccessPublic`), webhooks, jobs, runtime capabilities,
`/refresh`, and both asset endpoints (`/__air/assets/{name}` and `/static/{name}`).
Missing, incorrect, or ambiguous credentials return 401 before dispatch or run
creation. Missing runtime credentials fail startup/handler construction.

Airlock authenticates external callers and verifies configured webhook signatures,
then supplies the target app credential and authoritative caller/run attribution.
`AccessPublic` means public through Airlock, not unauthenticated access to the
container listener. External MCP and platform capability authorization belong to
the host; the app receives authenticated tool or scoped runtime invocations.

## Attribution

Airlock supplies a nested `wire.Caller` snapshot for the admitted actor, access,
user/initiator, and origin. Header-based deliveries use `wire.CallerHeader`
(`X-Airlock-Caller`), encoded with `wire.EncodeCallerHeader` as base64url JSON,
not a signed token. Runtime capability invocations and jobs use their validated
scoped protocol bodies. App-owned webhooks carry an application caller, not a
human user. Missing or malformed required attribution is an error, not an
anonymous/public fallback.
Application code reads `agentsdk.CallerFromContext(ctx)`, not private transport
headers; see [Caller identity](caller.md) for the public Go API.

The app token is shared in both directions. Its possession authenticates delivery
to the SDK, but native app code also has it. This is not a native-code sandbox,
and an app JWT is not proof of a human caller to Airlock. `CallerFromContext`
exposes host-attributed claims for app logic, not a transferable user credential.
The source header is not a standalone signed identity token. The public snapshot
contains no invocation receipts, job lease tokens, or credential coordinates.
The host must independently authorize app-to-host operations and derive trusted
origin from host-owned state rather than accepting arbitrary app assertions.

## App-To-Host Calls

Pass the handler's context to SDK operations. The SDK carries its admitted run
and invocation receipt through that context and sends the app bearer credential
on outbound requests. Copying caller identity, access, `Platform`, or run
IDs into a fresh context does not establish authority. `Caller.Origin().Interface`
identifies the initiating surface (such as `InterfaceChat`); `Origin.Platform` is
presentation metadata (such as `telegram`), not a privileged caller alias.

Airlock validates the current app credential generation and a dispatch-bound
256-bit receipt, or the exact current job ID/attempt/lease fence, before restoring
the run's live authority. Receipt plaintext is not persisted in shared storage;
the host stores its SHA-256 hash, run, app, generation, expiry and closure state.
Treat receipts as credentials: do not log them or put them in application data.
Closing a delivery receipt denies operational callbacks. Until expiry, a closed
but unrevoked receipt can authorize only fenced terminal completion of active
nonhosted work, without granting the initiating user's authority. Apps must not
complete the hosted run borrowed by a runtime capability invocation.

Interactive runs honor initiating token expiry. Durable user jobs ignore token
lifetime but still check the initiating account, session revocation, OAuth grant,
bridge link and current app grants. Logout revokes the session and can cancel
jobs initiated through it. Use typed jobs for durable work; do not retain a
handler context as a durable credential. App-created work, webhooks and crons have
app authority, not a human identity.

Native app code is trusted with its own database, bound resources and runtime
credentials. Context-scoped authorization protects host-mediated operations; it
does not sandbox native code from those app-owned resources. Public/member access
is a separate authorization decision and does not implement external user
authentication. `externalAuth` and external identity attributes/providers are
not implemented. JavaScript keeps its separate read-only `user` binding; the Go
caller snapshot does not add a JavaScript caller API.

## Health And Startup

Only the built-in `GET`/`HEAD /health` probe is unauthenticated. It checks database
reachability and reports registered tool/webhook names; it does not dispatch app
handlers or establish caller attribution. Other methods and paths, including
unknown paths and mux redirects, require authentication.

`AIRLOCK_AGENT_MODE=manifest` writes the declaration manifest to stdout without
opening a listener or requiring runtime credentials. There is no HTTP manifest
endpoint. Startup sync and the sync triggered by `/refresh` are authenticated
outbound app-to-Airlock calls.

`OnStart` hooks receive an application caller with startup execution. Caller
lookup is a local snapshot read: it never creates a run or performs network I/O.
Direct app background API calls create their own execution context without
mutating the supplied context. An arbitrary context does not acquire caller
state merely because it is passed to an SDK operation.

Sync and capability invocation require `wire.AppRuntimeProtocol` equal to
`airlock.app-runtime.v2`, independently of SDK semver. The manifest includes all
environment declarations. App-created code/background runs accept only trigger
metadata, never user, conversation, or access assertions. Runtime origin is
host-supplied, and upgrades require an existing admitted run ID. An incompatible
manifest or invocation is rejected and requires rebuilding the app with a
compatible SDK. There is no mixed v1/v2 runtime compatibility mode.

## HTTP Tests

`agenttest.New` installs `test-token` as the app credential. Send it explicitly
for every non-health HTTP request, including public routes and assets:

```go
user := agentsdk.User{ID: "00000000-0000-0000-0000-000000000001"}
req := httptest.NewRequest(http.MethodGet, "/", nil)
req.Header.Set("Authorization", "Bearer test-token")
req = req.WithContext(agenttest.WithUser(req.Context(), user))
agenttest.SetCallerHeader(req)
env.Agent.Handler().ServeHTTP(httptest.NewRecorder(), req)
```

`WithUser` and `WithCaller(ctx, user, access)` supply in-process human test
attribution. Use `agenttest.WithCallerInfo` with an explicit `agenttest.CallerInfo`
for anonymous/application tests; a plain context is not an anonymous fixture and
caller lookup panics on missing state. See [Caller identity](caller.md) for helper
examples. These helpers are not a bypass for delivery authentication.
`agenttest.SetCallerHeader(req)` encodes the injected snapshot for SDK HTTP
dispatch, including requests over an `httptest.NewServer` network boundary.
Runtime invocation tests supply their scoped protocol body. Standalone listeners
require their configured app token as well. Calling
an application handler function directly is a unit test, not SDK HTTP ingress.
