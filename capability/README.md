# Capability Catalogue

`capability` and `wire` can be imported without importing the root SDK package.

```go
defs, err := capability.Catalog(manifest, capability.Discovery{
    MCPSchemas: schemas,
})
```

Each `Definition` has `Path`, `Target`, `Access`, `Description`, `LLMHint`,
`InputSchema`, `OutputSchema`, `TextOutput`, and `InputExamples`. `TextOutput`
keeps plain-text file results as JS strings even when they contain valid JSON.
`Path.ID()` is the transport
identity: `<kind>/<escaped canonical namespace>/<escaped canonical operation>`.
Examples: `air//file_read`, `tool//calculate`, `conn/mail/request_json`.
`Path.JSParts()`, `Path.JS()`, and `Path.Direct()` give presentation names for
the same identity. Aliases, normalization, and name truncation never change IDs.
`Path.Kind()`, `CanonicalNamespace()`, and `CanonicalOperation()` expose dispatch
components without parsing an ID. The shared chat runtime uses these identities
for both JavaScript bindings and direct tools.

Build the complete catalogue before selecting definitions. Catalogue construction
rejects duplicate IDs and JS/direct aliases, including conflicting access levels.
Only manifest-declared external MCP servers admit discovery schemas.
Definitions describe capabilities, not authorization grants. `AccessPublic` on
file operations means directory-specific policy must still be checked.

## Inventory

`Fixed()` contains 30 entries. Builtin input contracts live in `inputs.go` and
are shared with the app/direct executors. No selected-definition list can add
an executor to an app.

| Target | Operations |
| --- | --- |
| App, 18 | `file_read`, `file_read_bytes`, `file_read_range_bytes`, `file_grep`, `file_head`, `file_tail`, `file_lines`, `file_stat`, `file_exists`, `file_write`, `file_delete`, `file_list`, `file_encode`, `file_decode`, `file_decode_text`, `file_edit_lines`, `file_sed`, `query_db` |
| Platform, 11 | `output`, `file_share_url`, `http_request`, `web_search`, `attach_to_context`, `analyze_image`, `transcribe_audio`, `generate_image`, `speak`, `embed`, `request_upgrade` |
| Executor, 1 | `air.log`; console methods are JS intrinsics, not app RPCs |

`Catalog` adds registered app tools, two platform operations per connection
(`request`, `request_json`), two per topic (`subscribe`, `unsubscribe`), discovered
MCP operations. Jobs, connectors, environment variables,
routes, model slots, and lifecycle hooks are author APIs/declarations, not
implicitly exposed model tools. Registered tools may use their author APIs.

## App Invocation

Send `wire.RuntimeInvokeRequest` as JSON to `POST /__air/runtime/invoke`
(`wire.RuntimeInvokePath`) using `Authorization: Bearer <target-agent-token>`.
Do not expose this credential or forward this internal route through public
ingress. There is no authentication fallback and no trust in user headers.

The broker loads an active run from shared storage, verifies that it belongs to
the target agent, applies the run's capability/registration policy, and fills
`RuntimeContext` from authoritative run records. This is a trusted internal
protocol, not a public API for clients to choose their own identity or access.
The app checks the token, matching agent ID, canonical run/context UUIDs,
explicit access, full manifest inventory, and registered-tool access. It executes
only registered tools and the fixed app operations. The broker handles platform
operations directly, without an app hop. `Input` is the canonical JSON tool input
object; the JS adapter accepts one object argument and unwraps its positional array.

`RuntimeContext` carries `AgentID`, `RunID`, `BridgeID`, `ConversationID`,
`Origin`, `CallerAccess`, `UserID`, `UserEmail`, `UserDisplayName`, `Platform`,
`SupportedModalities`, and optional `Job` (`ID`, `Attempt`, `LeaseToken`).
Execution borrows this run, honors HTTP cancellation and a bounded timeout,
and never creates or completes the run. Job progress and SDK API requests retain
the run and job attribution. Invocation-local file caches are cleaned up on return.

The response is one `wire.RuntimeInvokeResponse` JSON object, not NDJSON. It
retains `Output`, `Title`, `Metadata`, `Warnings`, `Attachments`, `Logs`, `Actions`,
and `Error`. Ordinary tool failures and panics return HTTP 200 with `Error`;
authentication, authorization, and protocol failures return non-2xx. Airlock owns
merging invocation telemetry into the active run and completing that run.

Attachments contain only `Path`, `MimeType`, and `Filename`. Existing `s3ref:`
attachments retain the checked object path without loading bytes; inline goai
attachments are persisted to object storage and returned as references. Failed
attachments produce warnings without discarding successful result fields.
The broker can turn these paths into the platform's attachment references for
model/session handling. Sensitive values, including agent and job credentials,
are redacted from response strings and nested metadata before serialization.
