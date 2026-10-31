# Direct resources and app integration validation

## Discover and use your resources directly

Use `resources` to discover resources available to your authenticated identity,
inspect safe metadata, or call HTTP/MCP providers without an app declaration or
binding:

```bash
go tool air resources list
go tool air resources inspect /personal_spotify
go tool air resources request /personal_spotify --path /v1/me/player/currently-playing
go tool air resources inspect /personal_github
go tool air resources mcp-tools /personal_github
go tool air resources mcp-call /personal_github --tool search_repos --data '{"query":"airlock"}'
```

`/name` uses the globally unique resource slug on the selected remote, not its
human display name or a provider name. `/UUID` selects the stable resource ID;
UUID-shaped selectors always mean IDs. Bare names/UUIDs are rejected, never
guessed. Names such as `/preview` and `/production` are ordinary global names.
The list contains only resources visible under your ownership/grants,
not every user's resources. Inspect requires view access; calls require live use
access. Credentials stay in Airlock's shared resource transport. HTTP requests
default to GET. These are real provider calls; only perform the authorized
operation, and never repeat an interrupted call just to discover its outcome.

Hosted development uses the injected, per-command execution credential on its
admitted Airlock host. No browser login, app binding or production-app execution
grant is needed. Original conversation/bridge/MCP restrictions, resource grants,
revocation and command-attempt fences remain live. App-originated delegation does
not acquire host resource authority. Local development uses your existing login
and selected/default remote; `--remote <name>` selects another saved remote.
Hosted remotes must match the injected host; credentials cannot be forwarded to
an arbitrary `--url`. Global addresses reject `--agent` and `--target`.
Build-bound codegen integration tokens cannot use direct resources.

## Verify an app's declared slots

Use the same `go tool air` commands from a bound local workspace or Airlock
codegen to validate configured dependencies without retrieving their
credentials:

```bash
go tool air resources list preview
go tool air resources inspect preview/spotify
go tool air resources request preview/spotify --path /v1/me
go tool air resources request production/spotify --path /v1/me
go tool air resources mcp-tools preview/github
go tool air resources mcp-call preview/github --tool search_repos --data '{"query":"airlock"}'
go tool air resources mcp-probe https://example.com/mcp
```

`target/slot` selects a declared app slot, not a global resource name. The logical
app must be unambiguously bound in the workspace/remote or selected with `--agent`.
`preview` selects the authenticated owner's saved preview; `production` selects
production. Other target labels pass through existing deployment resolution and
must already exist; the CLI does not create named deployments. A conflicting
`--target` is rejected. Use `--type connection` or `--type mcp_server` when the
same slot slug is declared with multiple types; the CLI never guesses.
Run `resources list preview` and `resources list production` first.
An integration definition and an authorized resource in your account do not
establish a binding: select that resource for the selected deployment's slot in
Airlock when verifying the app slot. For direct provider inspection, use
`resources request` or `resources mcp-*` instead; do not create an app binding
just to inspect your resource. Per-user slots require the authenticated caller's own binding; another
user's binding or the app owner's identity is never substituted. Preview and
production bindings are independent. A host error saying the slot is not
declared or not bound occurs before any provider request. An upstream failure
prints `HTTP <status>` and `resource returned HTTP <status>`. App-bound production
calls require the existing production execution approval in hosted development;
neither a global name nor an app-slot name grants authority.

```bash
go tool air resources bind preview/spotify --resource /personal_spotify
go tool air resources unbind preview/spotify
```

Binding compatibility and live resource use are checked by the host.
`resources bind` changes the selected deployed target, not a pending source candidate.
Missing, incompatible or ambiguous resources require user input, not guessing or borrowing
another user's connection. Production live binding edits remain protected
configuration administration, outside a production execution grant in hosted
development. Standalone operator logins retain ordinary app-admin configuration
gates. For hosted production, prepare selections with
`deploy --bind connection:spotify=UUID` in the approved immutable
production candidate. For initial/setup-blocked previews, inspect
`preview configuration <candidate-UUID>` and use `preview --bind connection:spotify=UUID`
to select the pending candidate; do not mutate an unrelated running target.

Required unbound slots do not block source deployment. Report unresolved slots
and the target's existing settings link, and distinguish deployed source from
operational readiness. Apply only compatible, unambiguous selections within the
user's authority; missing authority or a required choice needs user input, not a
pre-deployment configuration gate.

`resources mcp-probe` connects directly to a URL and reports the public tool list plus a
passive OAuth assessment. `auth.dynamicClientRegistration` is `advertised`,
`not_advertised`, or `unknown`; the probe never attempts to register a client.
When OAuth endpoints are discovered without DCR, the output marks
`MCPAuthOAuthDiscovery` unavailable and identifies `MCPAuthOAuth` as the likely
mode, including the discovered authorization and token URLs. An `unknown`
assessment means the auth mode must come from current server documentation; it
does not mean `MCPAuthNone`. `mcpStatus` is `connected` when the public handshake
succeeds and `authentication_required` when the endpoint returns 401; the latter
still returns the auth assessment with an empty tool list.

The other commands resolve a deployment target through
`.airlock/local/agent.toml` for local development or the build-bound environment
in hosted codegen. A named remote binds one Airlock URL and one stable agent ID,
so one checkout can address production and development agents on the same or
different instances:

```bash
go tool air deploy --remote dev --url https://airlock.example.com --agent my-agent-dev -m "Configure development integrations"
go tool air resources list preview --remote dev
go tool air resources request preview/spotify --remote dev --path /v1/me
go tool air resources mcp-tools preview/github --remote dev
```

Selecting `dev` does not change `default_remote`. Subsequent deploys and pulls
can use `--remote dev` without repeating the URL or agent. Run
`go tool air remote default dev` only when `dev` should become the workspace
default. An existing remote cannot be rebound to another URL or agent; use a
different remote name so source synchronization state stays attached to one
deployment target.

Addresses always state their scope explicitly. `resources list` lists global
resources; `resources list <target>` and `resources bindings --target <target>`
list that app target's slots without exposing credentials.

Airlock injects credentials for authenticated connection and MCP declarations.
Native Go no-auth handles use their fixed manifest endpoint directly and require no
resource setup or binding. Their `BindingMode` is optional and ignored; known
values remain accepted for source compatibility. Independently created no-auth
resources remain available through the direct resource namespace and retain
their resource grants.
App-slot calls require agent-admin access and independent live resource-use access.
Hosted development uses its verified execution credential; preview calls need
no separate browser login or production approval. Hosted codegen receives a short-lived
integration token that cannot deploy, update source, configure credentials, or
call unrelated agent APIs. In codegen, `resources list` lists only the build's
slots; use `<injected-runtime-UUID>/<slot>` addresses. Codegen cannot select a
different remote/target or resolve global resource names.

Connection response bodies are written to stdout. `resources mcp-tools` discovers the
authorized server's live tool schemas and instructions; `resources mcp-call` invokes the
live server. Both print JSON so their output can become sanitized test fixtures.
Connection and MCP results are capped at 20 MiB.

## Go connection response bodies

Use `ConnectionHandle.Request` and `RequestJSON` for small JSON-like API
responses. Both buffer the complete body in agent memory and enforce a 20 MiB
total-byte limit. `RequestJSON` does not stream JSON items.

Choose `RequestStream` before issuing a request when the response is a download,
will be processed as a file, may exceed 20 MiB, or has an unknown length. Do not
try `Request` first and fall back to a second streaming request: repeating a
request can duplicate a mutation or return different data. Close the returned
body, then use a streaming parser or `io.Copy` to a local `os.CreateTemp` file.
Seek the file back to the start before reading it in-process. Close it before an
external program opens its path, and arrange immediate deferred cleanup while
handling cleanup failures that matter to the operation. A local temporary file
is a processing cache, not durable Airlock file storage.

Successful native connection responses stream through Airlock without a fixed
response-size limit. The host's 30-second outbound network deadline still
applies. Request bodies remain bounded, and non-2xx responses retain only the
bounded SDK error preview described below. JavaScript and MCP calls keep their
separate buffered limits.

## Go connection errors

`ConnectionHandle.Request`, `RequestStream`, and `RequestJSON` preserve non-2xx
responses as `*agentsdk.ConnectionHTTPError`. Use `errors.As`, not error-string
parsing:

```go
var httpErr *agentsdk.ConnectionHTTPError
if errors.As(err, &httpErr) {
    if httpErr.Source == agentsdk.ConnectionErrorSourceUpstream && httpErr.StatusCode == 422 {
        // Inspect httpErr.Body for the API's domain validation response.
    }
    return err
}
```

`Source` is `host`, `upstream`, or `unknown`. `unknown` means the host did not
send the trusted response discriminator, including hosts that do not implement
structured connection errors. `Code` is populated only for host errors such as
`not_bound`, `forbidden`, `invalid_request`, `gateway`, and `gateway_timeout`.
An upstream 401, 402, 403, 422, or 5xx remains an upstream
`ConnectionHTTPError`; only an explicit host `authorization_required` response
becomes `*agentsdk.AuthRequiredError`.

`Body` preserves up to 4096 bytes for domain details, with `BodyTruncated`
reporting overflow. `Error()` never includes `Body`, so ordinary logs do not
expose response data. Successful responses retain their upstream status,
headers, and body; the internal discriminator header is not exposed.

Status alone does not establish retry or idempotency safety, and the SDK does
not retry connection requests automatically. A host `gateway_timeout` supports
`errors.Is(err, context.DeadlineExceeded)`, but does not assert whether the
upstream observed a mutating request. Local request cancellation and deadlines
continue to preserve their ordinary `errors.Is` behavior.

JavaScript connection callbacks currently report capability failures through
the JavaScript tool-error channel rather than this Go error type. MCP tool calls
are also unchanged: a valid MCP `CallToolResult` retains `IsError`, `Content`,
and `StructuredContent` as tool-result data rather than becoming an HTTP error.
