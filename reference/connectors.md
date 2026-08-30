# Connectors

Connectors are pure-Go programs installed on another machine. They make an
outbound HTTPS connection to Airlock and expose a fixed contract of typed
commands and confined local directories. Airlock owns resource binding,
authorization, persistence, auditing, and multi-target orchestration. Connector
code remains the local security boundary.

## Repository layout

Each immediate child of `connectors/` is an intentional `main` package:

```text
connectors/transmission/main.go
torrentcontract/contract.go
torrent/service.go
```

Keep shared definitions in a non-`main` package so the agent and connector use
the same Go values. `go tool air build` discovers immediate children, builds and
runs each native binary with `AIRLOCK_CONNECTOR_MODE=manifest`, validates its
manifest, and cross-compiles every declared target with `CGO_ENABLED=0`.

## Shared contract

Contract IDs are required, explicit reverse-domain identifiers. Choose a stable
namespace controlled by the organization, Airlock host, username, agent, or
connector, such as `com.example.media.transmission`.

Go does not support generic methods, so command definitions use the generic
package function `connector.DefineCommand`:

```go
package torrentcontract

import "github.com/airlockrun/agentsdk/connector"

var Contract = connector.DefineContract("com.example.media.transmission")

type AddInput struct { Magnet string `json:"magnet"` }
type Torrent struct { ID string `json:"id"` }

var Add = connector.DefineCommand[AddInput, Torrent](Contract, "add_torrent", connector.CommandOptions{
    Revision: 1,
    Description: "Add a magnet to Transmission.",
})

var Completed = connector.DefineDirectory(Contract, "completed", connector.DirectoryOptions{
    Revision: 1,
    Description: "Completed torrent files.",
    Read: true,
    List: true,
})
```

Schemas are reflected deterministically from JSON-tagged Go types. Structural
schema SHA-256 hashes, revisions, command mode, contract ID, and directory
access are matched exactly. Change a revision when structure or semantics are
incompatible. Descriptions do not affect schema hashes.

Contract types deliberately reject custom JSON/text marshalers, anonymous or
conflicting fields, `json:",string"`, interfaces, `[]byte`, and
`json.RawMessage`. These shapes make reflected schemas diverge from
`encoding/json`; transfer bytes through connector directories instead.
Wire-visible command types also reject architecture-sized `int`, `uint`, and
`uintptr` values. Use explicit-width integer types throughout command inputs
and outputs, including nested structs, pointers, arrays, slices, and map values.

## Agent declaration and calls

```go
host := agent.RegisterConnector(&agentsdk.Connector{
    Slug: "torrent_host",
    Description: "Torrent server used for downloads.",
    Requires: connector.Require(torrentcontract.Add, torrentcontract.Completed),
})

result, err := agentsdk.CallConnector(ctx, host, torrentcontract.Add,
    torrentcontract.AddInput{Magnet: magnet})

files, err := host.List(ctx, torrentcontract.Completed,
    protocol.DirectoryListRequest{Path: "", Limit: 100})
```

Use `StartConnectorJob` with a caller-generated `uuid.UUID`, then
`ConnectorJobHandle.Get`, `Wait`, and `Cancel` for
commands declared with `Mode: protocol.CommandModeJob`. A need with `Multiple:
true` may use typed `StartConnectorOrchestration` with a caller-generated
request UUID and explicit canary count. Its durable handle supports `Get`,
`Wait`, and `Cancel` and exposes every child and its progress history. Airlock
performs and persists parallel, serial, rolling, canary, or quorum execution.
Do not create fleet orchestration goroutines in the agent.

Directory methods are `List`, `Stat`, `Read`, `Write`, `Delete`, `Move`,
`Import`, and `Export`. Import and export ask Airlock for narrow transfer grants;
the connector accepts only HTTPS URLs on origins approved during activation and
rejects cross-origin redirects.

## Connector binary

```go
func main() {
    settings := &Settings{}
    runtime := connector.New(connector.Config{
        Kind: "transmission",
        Contract: torrentcontract.Contract,
        Name: "Transmission Connector",
        Description: "Controls local Transmission and exposes completed files.",
        ArtifactVersion: "1.0.0",
        ServiceMode: connector.ServiceUser,
        Targets: []string{
            connector.PlatformLinuxAMD64, connector.PlatformLinuxARM64, connector.PlatformLinuxARMv7,
            connector.PlatformDarwinAMD64, connector.PlatformDarwinARM64,
            connector.PlatformWindowsAMD64, connector.PlatformWindowsARM64,
        },
        Settings: settings,
        SelfTest: func(ctx context.Context) error { return testTransmission(ctx, settings) },
    })
    torrentcontract.Add.Handle(runtime, addTorrent)
    torrentcontract.Completed.Handle(runtime, connector.LocalDirectory(settings.CompletedDir))
    if err := runtime.Run(); err != nil { log.Fatal(err) }
}
```

Use direct Go APIs or typed process arguments. Never interpolate request values
into a shell. Expose narrow commands and only deliberately selected local
directories. Keep Airlock-built connectors compatible with `CGO_ENABLED=0`.
Integrations requiring CGO or native toolchains must be distributed externally.

## Local settings and activation

Settings are a pointer to a struct. Supported `connector` tag kinds are
`string`, `secret`, `bool`, `integer`, `duration`, `url`, `file`, `directory`,
and `enum`. Options include `required`, `default=`, `enum=a|b`, `name=`, and
`description=`. Secrets must use `connector.Secret`; noninteractive
configuration accepts only `--<name>-file` or `--<name>-stdin` for them.
Secrets cannot declare defaults, and setting names cannot shadow lifecycle or
generated secret flags.
Settings are flat direct struct fields; nested structs and collection types are
unsupported. Integer settings use fixed-width signed types such as `int32` or
`int64`; architecture-sized `int`, `uint`, and `uintptr` values are rejected.

`ServiceMode` is required: select `connector.ServiceUser` for a user systemd
unit, macOS LaunchAgent, or Windows logon task, or `connector.ServiceSystem` for
a dedicated Linux or Windows system service. macOS targets require
`connector.ServiceUser`; macOS system services are unsupported.
The selection is persisted in draft and activated installation state; a binary
configured for another mode fails rather than reinterpreting existing state.

`configure` validates all fields and runs the complete self-test before an
atomic save. User-mode validation runs as the invoking user. System-mode
configuration provisions state access first and runs validation as the eventual
service identity: a connector-specific system account on Linux or LocalService
through a temporary SCM service on Windows. Configuration and activation fail
if that identity cannot execute the binary, decrypt/read settings, access local
paths, or pass the self-test. This preserves the explicit `configure`,
`activate`, then `install` decision sequence without validating under a more
privileged identity than the installed service. Unix state directories and
files use `0700` and `0600`; Windows
installation credentials and pending activation state use DPAPI. Settings,
credentials, local paths, and activation tokens stay outside source and binary
artifacts.

Each approved installation is stored beneath `installations/<installation-id>`
and has its own service, settings, credential, idempotency records, and runtime
status. Use `--installation <uuid>` or
`AIRLOCK_CONNECTOR_INSTALLATION_ID=<uuid>` when more than one installation of a
kind exists; ambiguous commands fail. Use `configure --new` followed by
`activate --new` to create another installation. Windows system services use
ProgramData, machine-scope DPAPI, and an ACL restricted to LocalService, SYSTEM,
and Administrators.

Activation supports `--no-browser`, `--no-wait`, `--wait`, and `--check`.
Interactive terminals open a browser and wait by default. Without a TTY,
activation saves protected pending state, prints the approval instructions and
follow-up `--check` command, and exits. Only explicit `--wait` can block without
a TTY.

## Lifecycle and runtime

The binary provides `activate`, `run`, `install`, `uninstall`, `start`, `stop`,
`restart`, `status`, `configure`, `validate`, `version`, `unregister`, `upgrade`, `rollback`,
`enable`, `disable`, and `reconcile-job`. Use `reconcile-job <idempotency-id>
--output-json <json>` or `--error <message>` to resolve a locally indeterminate
side effect after checking the external system. Linux supports system and user systemd services.
Windows supports a LocalService system service and a user logon task. macOS
supports per-user LaunchAgents only.

### macOS artifacts, launchd, and TCC

Airlock-built macOS connector artifacts are unsigned and not notarized. Verify
the artifact's published SHA-256 before changing any extended attributes. If
Gatekeeper quarantines that verified artifact, remove quarantine from that one
file only:

```bash
shasum -a 256 /path/to/connector
xattr -d com.apple.quarantine /path/to/connector
```

Do not recursively clear quarantine and do not clear it before checksum
verification. Installation copies the verified executable bytes to a managed
path, so the installed copy does not retain the downloaded file's quarantine
attribute.

User mode stores state under
`~/Library/Application Support/Airlock/Connectors/<kind>`, installs the binary
beneath the selected installation's `bin` directory, and writes
`~/Library/LaunchAgents/run.airlock.connector.<kind>.<installation-id>.plist`.
It bootstraps the service in `gui/<uid>` and is the appropriate mode for apps,
automation, and files governed by the signed-in user's desktop session.

Lifecycle operations use `launchctl bootstrap`, `bootout`, `kickstart`, `print`,
`enable`, and `disable` against the explicit `gui/<uid>` launchd domain. Status
is active only when `launchctl print` reports `state = running`; stale registered
jobs remain inactive and lifecycle commands clean them up safely.

macOS privacy controls are not bypassed by installation. A user LaunchAgent runs
in the graphical user's domain, but the user must still grant each requested
privacy permission to the installed executable where macOS supports that access,
including Contacts, Photos, Accessibility, Screen Recording, Automation, camera,
or microphone. Unsigned binary
replacement can cause macOS to require permission again. Keep TCC-sensitive
connectors in user mode and expose only the narrow resources the integration
needs.

## Upgrade and rollback

`upgrade` compares the installed settings schema with the candidate schema.
Values are retained only when the setting name and kind remain compatible;
removed settings are dropped, changed settings use candidate defaults or new
typed flags, and newly required settings use the candidate's ordinary setting
flags and prompts. For example, use `upgrade --non-interactive --endpoint
https://host --token-file /secure/token`. A non-TTY invocation never prompts and
reports the exact missing `--name`, `--name-file`, or `--name-stdin` flags.
Do not run candidate `configure` before `upgrade`.

`install` fails when the service is already installed and directs the operator
to `upgrade`. Upgrade and rollback require an enabled installation; run `enable`
first when an installation is disabled.

Candidate settings are written to protected staging files and pass the complete
self-test under the eventual service identity before the installed service is
stopped. Upgrade then retains the installed binary, settings, settings schema,
and installation state, stops the old service, replaces the binary and staged
settings, and starts the candidate. The old binary is never started with the
candidate settings schema. A failed replacement or readiness check restores the
complete retained state, starts the old binary, and verifies that its executable
digest reconnects. `rollback` performs the same stop-owned sequence explicitly
and fails if no complete retained rollback exists. System-service executables
live outside service-writable state.

macOS rollback slots bind the retained binary and LaunchAgent plist to the exact
generic rollback-state generation containing settings, installation state, and
credentials. Rollback fails without changing the service when any generation or
artifact digest differs.

The runtime computes the lowercase SHA-256 digest of the executable bytes it is
actually running. Activation, every interface publication, and every heartbeat
include that `artifactDigest`; readiness and lifecycle verification also bind to
the digest so equal version labels cannot hide different binaries.

The runtime publishes its interface, sends heartbeats, and long-polls over
outbound HTTPS. Dispatch is closed by registered operation name and checks exact
revision, mode, input/output schema identity, deadlines, input/output limits,
and cancellation. Cancellation uses an independent poller and is not blocked by
work concurrency.
Stable idempotency IDs have durable local records. A crash after an operation
starts but before completion leaves an explicit `indeterminate` outcome rather
than silently repeating a side effect.

Control-plane clients reject every redirect so installation credentials never
leave the exact activated Airlock origin. Activation records and displays the
proposed storage origins before approval and rejects an approval whose origins
drift. Storage transfers permit only locally approved HTTPS origins and reject
cross-origin redirects.

`connector.LocalDirectory` uses `os.Root` descriptor/handle-relative
containment, canonical relative paths, ranged bounded reads, bounded listings,
safe modes, and same-root atomic replacement. It does not expose arbitrary
filesystem providers.
