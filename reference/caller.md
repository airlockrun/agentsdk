# Caller Identity

Go handlers read the caller attached to their execution context:

```go
caller := agentsdk.CallerFromContext(ctx) // r.Context() in an HTTP handler
user, ok := caller.User()
if !ok {
    return errors.New("a human user is required")
}
// Scope user-owned app data by user.ID, not email or display name.
```

`Caller` is a value snapshot with private state. It exposes these methods:

| Method | Meaning |
| --- | --- |
| `Kind() CallerKind` | `CallerAnonymous`, `CallerUser`, or `CallerApplication`: who the work acts as. |
| `Access() Access` | App access: `AccessPublic`, `AccessUser`, or `AccessAdmin`. Not an identity or platform membership check. |
| `User() (User, bool)` | The human the work acts as, present for a user caller. |
| `Initiator() (User, bool)` | The human who initiated the work, when present. Attribution, not an alternate authorization identity. |
| `Origin() Origin` | Source interface, platform/client metadata, and execution kind. |

The snapshot is not a credential or a live authorization query. Its fields are
private, its zero value is invalid, and returned user/origin values do not mutate
execution state. Host operations independently restore and revalidate the
admitted run's authority.
Invocation receipts, job lease tokens, sessions, and credential coordinates are
not exposed through this API. Do not serialize a snapshot as proof of authority.

## User And Access

`User` exposes `ID string`, `Email string`, `DisplayName string`, and
`PlatformMember bool`. `ID` is the stable internal user ID for scoping app data;
email and display name are display claims, not authorization keys.

Every actual admitted human has `PlatformMember = true`. Platform membership
does not imply membership of this app: a user can have `AccessPublic`. Do not
infer identity from access or infer platform membership from an access tier.
Use `User()`'s bool for human presence and `Access()` for app access.
There are no external identity attributes, provider APIs, or `externalAuth`
implementation.

Anonymous callers have no human user. Application callers act as the app, not
its owner. App-owned webhooks, crons, and native work have neither a human user
nor a human initiator; never fill either from the app owner. There is no app-owned
launch-with-human-initiator feature. User-initiated jobs remain user work and
preserve their user initiator.

## Origin

`agentsdk.Origin` keeps the initiating surface separate from how work executes:

- `Interface agentsdk.Interface`: `InterfaceHTTP`, `InterfaceChat`,
  `InterfaceMCP`, `InterfaceSchedule`, or `InterfaceApplication`.
- `Platform string`: presentation metadata such as `telegram` for a linked bridge.
- `ClientID string`: source client metadata, when applicable.
- `Execution agentsdk.ExecutionKind`: `ExecutionRequest`, `ExecutionJob`,
  `ExecutionBackground`, or `ExecutionStartup`.

`InterfaceUnknown` and `ExecutionUnknown` are available for explicitly injected
tests, not missing-context fallbacks.

A bridge chat tool is chat-origin work, even though host-to-app delivery uses
HTTP. A user job preserves its source interface while its execution is Job.
A scheduled job is application-owned with Schedule origin and Job execution.
Startup hooks have an application caller with Application origin and Startup
execution. Platform and client labels do not establish identity or authority.

## Context Lifetime

Use the context provided to tools, webhooks, jobs, and `OnStart` hooks; HTTP
handlers use `r.Context()`. Pass it through to SDK operations. Caller lookup
never materializes a lazy run, creates a background run, or performs network I/O.

`CallerFromContext` panics if the context has no framework or explicitly injected
test caller state. A plain `context.Background()` or a nil context is not an
anonymous caller. Do not recover this panic to guess anonymous access.

Direct app background API calls create an application execution context for
their work without mutating the input context. Calling an SDK operation with a
plain context does not make a later caller lookup on that context valid.
Definition factories and offline manifest inspection have no runtime caller.
Use typed jobs for durable work rather than retaining a request context as a
credential.

## Tests

Import `github.com/airlockrun/agentsdk/testutil` and inject human callers
explicitly, including when calling a handler or service method directly:

```go
user := wire.CallerUser{ID: "00000000-0000-0000-0000-000000000001", PlatformMember: true}
ctx := testutil.WithCaller(t.Context(), wire.Caller{
    Kind: "user", Access: wire.AccessUser, User: &user, Initiator: &user,
    Origin: wire.CallerOrigin{Interface: "http", Execution: "request"},
})
got, ok := agentsdk.CallerFromContext(ctx).User()
if !ok || got.ID != user.ID || !got.PlatformMember {
    t.Fatalf("unexpected caller user: %+v, present=%t", got, ok)
}

publicUserCtx := testutil.WithCaller(t.Context(), wire.Caller{
    Kind: "user", Access: wire.AccessPublic, User: &user, Initiator: &user,
    Origin: wire.CallerOrigin{Interface: "http", Execution: "request"},
})
publicUser := agentsdk.CallerFromContext(publicUserCtx)
if publicUser.Kind() != agentsdk.CallerUser || publicUser.Access() != agentsdk.AccessPublic {
    t.Fatal("expected a user with public app access")
}
```

`testutil.WithCaller(ctx, wire.Caller)` validates and copies the explicit snapshot
without runtime, credential, DB or network lookup. Kind/access/interface/execution
are required; human fixtures require matching user and initiator snapshots with
explicit platform membership. Anonymous/application callers do not receive a
substituted app owner. Use explicit `unknown` origins when unspecified:

```go
anonymousCtx := testutil.WithCaller(t.Context(), wire.Caller{
    Kind:   "anonymous",
    Access: wire.AccessPublic,
    Origin: wire.CallerOrigin{
        Interface: "http",
        Execution: "request",
    },
})
if _, ok := agentsdk.CallerFromContext(anonymousCtx).User(); ok {
    t.Fatal("anonymous caller has a user")
}

applicationCtx := testutil.WithCaller(t.Context(), wire.Caller{
    Kind:   "application",
    Access: wire.AccessAdmin,
    Origin: wire.CallerOrigin{
        Interface: "application",
        Execution: "background",
    },
})
if _, ok := agentsdk.CallerFromContext(applicationCtx).Initiator(); ok {
    t.Fatal("app-owned work has a human initiator")
}
```

Caller injection does not bypass delivery authentication or grant host authority.
Context values do not cross network boundaries. SDK Handler/startup/delivery tests
use a real remote Airlock target and host-owned credentials/receipts. See
[remote development](remote-development.md) for supported operations.

## Source Migration

Human call sites must use `agentsdk.CallerFromContext(ctx).User()` and handle its
two results. There is no `UserFromContext` compatibility API. Tests must supply
caller state explicitly, including anonymous/application cases; missing state
is a programming error, not an absent user. Use `Kind()` when application and
anonymous callers require different behavior, not just `!ok` from `User()`.

This is a breaking Go source requirement. A dependency version must actually
contain this API before a standalone app can compile these call sites. Do not
pin an unpublished HQ workspace version in a standalone app or assume workspace
overlays apply to managed apps. Source call sites require manual migration when
the app adopts a resolvable compatible SDK; a dependency bump alone does not
rewrite them.

JavaScript has its separate read-only `user` binding (null when absent). This Go
API does not add a JavaScript caller binding or change the JS user shape.
