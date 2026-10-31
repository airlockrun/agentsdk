# Application environment configuration

`RegisterEnvVar` declares a target-local ordinary value or write-only secret.
`EnvVarHandle.Get(ctx)` reads the immutable configuration supplied when the
process starts; it does not fetch configuration or refresh it dynamically.
Startup hooks and later calls see the same snapshot. An explicitly configured
empty string overrides a declared default. An unset slot uses its declaration's
default, or an empty string. `Pattern` validates that effective value; a pattern
rejecting an empty string declares a nonempty requirement. Secrets cannot declare
defaults and join the SDK redact set before startup hooks execute.

Manifest inspection and the explicit validation, down-to and up-only migration
modes do not load runtime environment snapshots. Migration-only processes exit
before host synchronization and startup hooks. Environment handles are unavailable
there; migration validation must not depend on live configuration or secrets.

Use the existing Preview or Production settings pane to set, rotate or clear
declared slots. Saving never restarts a runtime. A running target displays
**Configuration changed—restart required** until its explicit restart produces a
ready replacement with the desired revision. Stopped targets use saved values on
their next start. An edit during startup remains pending; unsuccessful startup
does not advance the applied revision.

`go tool air env list` includes declared secrets as set/unset metadata only.
`env get` reads ordinary values only; `env set` and `env clear` save declared
slots and explain restart requirements. `go tool air status --json` exposes
desired/applied environment revisions and configuration state without values.
Hosted production mutations retain the server's operation-approval gates.

The host and apps require matching `airlock.env-config.v1` support. Rebuild apps
with the compatible SDK; loaded-revision status is unknown until a compatible
runtime reaches readiness. The host's private startup payload must never appear
in logs, build input, operator metadata or source attachments. No additional SDK
runtime protocol version is required beyond `airlock.app-runtime.v4`.

Production configuration is available after the deployment exists. Missing
configuration is informational; it does not introduce a build/deployment gate.
