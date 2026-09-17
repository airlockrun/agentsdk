# Notifications and User Directory

## Topic Declaration

```go
alerts := agent.RegisterTopic(&agentsdk.Topic{
    Slug:        "alerts",
    Description: "System alerts",
    Access:      agentsdk.AccessUser,
    Enrollment:  agentsdk.TopicEnrollmentDefaultOn,
})

err := alerts.Publish(ctx, []agentsdk.DisplayPart{
    {Type: "text", Text: "Daily report is ready"},
    {Type: "file", Source: "reports/daily.pdf", Filename: "report.pdf"},
})
```

`Description` and `Access` are required. `Enrollment` accepts
`TopicEnrollmentDefaultOn` or `TopicEnrollmentDefaultOff`; omission means off.
`AccessPublic` allows authenticated users without app membership. Live account,
app access, bridge binding, and linked identity are checked at delivery time.

Enrollment is a per-user preference, separate from bridge conversation routes.
With no override the topic default applies. Enabled users without a usable route
receive one automatic route to their most recently active eligible bridge
conversation. Notifications do not count as user activity, and opening another
conversation does not replace a usable route.

`topic.<slug>.subscribe()` enables enrollment. From a bridge it also adds the
current conversation as an explicit route, removes the automatic route, and
preserves other explicit routes. `unsubscribe()` removes the current bridge
route; removing the last route disables enrollment. In web chat, subscribe only
changes the preference and unsubscribe disables enrollment globally and removes
all routes. Deleting a conversation does not erase the user's preference.

`PerUser: true` prohibits `Publish`; use `PublishToUser(ctx, userID, parts)` with
an internal user UUID. Broadcast targets effectively enrolled users with valid
bridge routes, not all directory users. Delivery errors are returned; a partial
delivery or timeout must not be blindly retried because some parts or routes may
already have received it. There is no automatic resend queue or fallback after a
send attempt.

Web chat is not a durable notification destination. Each recipient gets at most
one best-effort live mirror, attached only to their currently open app
conversation. It is not persisted in web history or replayed on reconnect.
Bridge notifications are stored in the bridge transcript but excluded from LLM
context.

## ListUsers

`users, err := agent.ListUsers(ctx)` returns the tenant-wide human directory as
`[]agentsdk.DirectoryUser`, containing only `ID`, `Email`, and `DisplayName`.
This matches the platform's human-selectable directory scope, including users
without app grants. The current app credential authorizes the request; startup
and application-owned contexts work without substituting the app owner or a
human caller. IDs are addresses, not authority. Notification eligibility and
enrollment remain separate checks. The directory is unpaginated, matching the
platform directory API, and is not exposed as a JavaScript capability.
