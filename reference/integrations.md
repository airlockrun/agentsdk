# Live integration validation

Use the same `go tool air` commands from a bound local workspace or Airlock
codegen to validate configured dependencies without retrieving their
credentials:

```bash
go tool air integrations list
go tool air connection request spotify --path /v1/me
go tool air exec run ci-runner -- kick-build --branch main
go tool air mcp probe https://example.com/mcp
go tool air mcp tools github
go tool air mcp call github search_repos --args '{"query":"airlock"}'
```

`mcp probe` connects directly to a URL and is useful before registration. The
other commands resolve the current agent through `.airlock/local/agent.toml`
for local development or the build-bound environment in hosted codegen.

Airlock injects connection and MCP credentials and performs SSH authentication.
Local calls require agent-admin access. Hosted codegen receives a short-lived
integration token that cannot deploy, update source, configure credentials, or
call unrelated agent APIs.

Connection response bodies are written to stdout. Exec preserves remote stdout
and stderr and returns a non-zero local status when the remote command fails.
`mcp tools` prints Airlock's cached input schemas; `mcp call` invokes the live
server. Both print JSON so their output can become sanitized test fixtures.
Connection, exec-stream, and MCP results are capped at 20 MiB.
