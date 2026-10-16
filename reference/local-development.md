# Laptop development

Employees author app source using their preferred laptop editor/Codex/Claude or
the Airlock web developer. The laptop needs Go and pinned generation tools,
not Docker, PostgreSQL or a local host runtime.

* `go tool air toolchain install` installs the pinned build tools and references.
* `go tool air build` generates assets/schema code, runs deterministic unit tests
  and compiles. It does not start test infrastructure.
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
