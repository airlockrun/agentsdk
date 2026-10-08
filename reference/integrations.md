# Live integration validation

Use the same `go tool air` commands from a bound local workspace or Airlock
codegen to validate configured dependencies without retrieving their
credentials:

```bash
go tool air integrations list
go tool air connection request spotify --path /v1/me
go tool air mcp probe https://example.com/mcp
go tool air mcp tools github
go tool air mcp call github search_repos --args '{"query":"airlock"}'
```

`mcp probe` connects directly to a URL and reports the public tool list plus a
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
so one workspace can address production and development agents on the same or
different instances:

```bash
go tool air deploy --remote dev --url https://airlock.example.com --agent my-agent-dev -m "Configure development integrations"
go tool air integrations list --remote dev
go tool air connection request spotify --remote dev --path /v1/me
go tool air mcp tools github --remote dev
```

Selecting `dev` does not change `default_remote`. Subsequent deploys and pulls
can use `--remote dev` without repeating the URL or agent. Run
`go tool air remote default dev` only when `dev` should become the workspace
default. An existing remote cannot be rebound to another URL or agent; use a
different remote name so source synchronization state stays attached to one
deployment target.

Airlock injects credentials for authenticated connection and MCP declarations.
No-auth declarations use their fixed manifest endpoint directly and require no
resource setup or binding. Their `BindingMode` is optional and ignored; known
values remain accepted for source compatibility. Independently created no-auth
resources remain available through the direct resource namespace and retain
their resource grants.
Local calls require agent-admin access. Hosted codegen receives a short-lived
integration token that cannot deploy, update source, configure credentials, or
call unrelated agent APIs. Hosted codegen is fixed to its build-bound target and
does not accept local target-selection flags.

Connection response bodies are written to stdout. `mcp tools` prints Airlock's
cached input schemas; `mcp call` invokes the live
server. Both print JSON so their output can become sanitized test fixtures.
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
