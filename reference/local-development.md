# Laptop development

Employees author app source using their preferred laptop editor/Codex/Claude or
the Airlock web developer. The laptop needs Go and pinned generation tools,
not Docker, PostgreSQL or a local host runtime.

* `go tool air toolchain install` installs the pinned build tools and references.
* `go tool air build` generates assets/schema code, runs deterministic unit tests
  and compiles. It inspects the compiled app's canonical manifest offline, warning
  on stderr if no tools are registered. That warning does not fail intentional
  UI-only apps; manifest inspection errors do fail the build. It does not start
  test infrastructure or the app runtime.
* `go tool air test` runs deterministic tests. Runtime simulator/testcontainers
  imports in the default app source/test selection are rejected explicitly.
* `go tool air preview -m "Describe the candidate" --wait` uploads all candidate
  source/tests/fixtures to the selected real Airlock and starts your personal app.
* App HTTP, registered tool/agent execution, DB, files, migrations, sync, OnStart,
  job scheduling and Deno integration execute through real Airlock services.

Named remotes use `.airlock/local/agent.toml`; `--remote` selects the instance and
`--target preview|production` independently selects its deployment. General app
operations default to production. `air preview` creates or updates the saved
preview per app/remote without changing that default. An explicit `--target preview`
requires a saved reference. Copied/moved configuration can retain that reference;
access is authorized by the server. Inside a real hosted development workspace,
the host-injected preview is the default, with explicit production subject to host permissions.

See [remote development](remote-development.md) for actual command/API support,
source CAS, atomic candidate bindings, HTTP fixture packaging and remote suites.
`air run --local` is unsupported; the SDK contains no simulated host or local app
launcher. No local-state conversion or data cleanup is part of the workflow.

Platform developers may explicitly run a full actual Airlock dev stack using
the platform's compose/host tools. This is separate from SDK employee defaults.

## Build inputs and generated files

Run `go tool air build` after changing Go source, queries, `.templ` or
`styles/app.css`. The chain removes generated sqlc files, runs `go mod tidy`,
generates sqlc when `db/queries/*.sql` exists, runs templ and Tailwind, runs
`go test -p=1 -count=1 ./...`, then compiles and inspects the app manifest.
It also validates immediate `connectors/<slug>` manifests and cross-compiles
their declared targets with `CGO_ENABLED=0`. App binaries and connector output
live in a temporary directory removed after the build.

Commit `.templ`, `styles/app.css`, migrations, query SQL and `internal/db/doc.go`.
Do not commit regenerated `*_templ.go`, `views/static/app.css`, other generated
`internal/db/*`, ad-hoc `/agent` or `/agent.exe` binaries, or `.airlock/toolchain/`.
The generated Dockerfile rebuilds code/assets from these inputs.

## Importing a non-Go service

Keep application/domain logic in its original language and retain a thin Go
SDK wrapper. The wrapper registers HTTP routes and typed chat tools, performs
caller authorization and delegates both surfaces to the service. Go registrations
remain authoritative for capabilities; packaging files do not declare entrypoints
or SDK operations.

Select runtime source/assets in root `airlock.toml`, preserving repository-relative
paths under `/app`:

```toml
[build]
runtime_files = ["python-app/", "templates/", "static/"]
```

Use `setup.sh` for language runtimes, system packages and dependencies; it runs as
root at image build time after selected files are available under `/app`. It can
read `/app/python-app/requirements.txt`. Never edit the generated Dockerfile.
In the toolserver, verify installation with `sudo run-setup` and the installed
binary's version command. `/var/agent/bin/` is on the runtime PATH.

Keep the SDK listener on `:8080`, and the imported service on a separate localhost
port. Start and supervise the subprocess in a named `OnStart` hook with its working
directory under `/app`. Bound readiness waiting, fail startup on errors, and stop
the process when the hook context is canceled. Do not launch it in `newAgent()`
or during offline manifest inspection.
