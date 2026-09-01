# Connectors

Connectors are pure-Go child programs supervised by `airlock-host`. A connector
exposes a fixed contract of typed commands and confined local directories. The
host owns enrollment, the Airlock credential, control-plane HTTP, installation,
updates, rollback, process supervision, and local access policy. A connector
binary never connects to Airlock directly and has no standalone service mode.

## Repository layout

Each immediate child of `connectors/` is a `main` package:

```text
connectors/transmission/main.go
torrentcontract/contract.go
torrent/service.go
```

Keep shared definitions in a non-`main` package so the agent and connector use
the same Go values. `go tool air build` discovers immediate children, inspects
each native manifest, and cross-compiles every declared target with
`CGO_ENABLED=0`.

## Shared contract

Contract IDs are explicit reverse-domain identifiers. Generic package functions
define commands because Go does not support generic methods:

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
schema hashes, revisions, command mode, contract ID, and directory access are
matched exactly. Change a revision when structure or semantics are incompatible.
Descriptions do not affect schema hashes.

Contract types reject custom JSON/text marshalers, anonymous or conflicting
fields, `json:",string"`, interfaces, `[]byte`, `json.RawMessage`, and
architecture-sized integers. Use fixed-width integers and transfer bytes through
connector directories.

## Agent declaration

```go
host := agent.RegisterConnector(&agentsdk.Connector{
    Slug: "torrent_host",
    Description: "Torrent server used for downloads.",
    Requires: connector.Require(torrentcontract.Add, torrentcontract.Completed),
})

result, err := agentsdk.CallConnector(ctx, host, torrentcontract.Add,
    torrentcontract.AddInput{Magnet: magnet})
```

Use `StartConnectorJob` for job-mode commands. Multi-target needs may use
`StartConnectorOrchestration`; Airlock persists and coordinates parallel,
serial, rolling, canary, and quorum execution.

Directory methods are `List`, `Stat`, `Read`, `Write`, `Delete`, `Move`,
`Import`, and `Export`. Imports and exports use narrow Airlock transfer grants.
The child accepts only HTTPS origins supplied by the host during initialization
and rejects cross-origin redirects.

## Connector child

```go
func main() {
    if err := newConnector().Run(); err != nil { log.Fatal(err) }
}

func newConnector() *connector.Runtime {
    settings := connector.DefineSettings[Settings]()
    runtime := connector.New(connector.Config{
        Kind: "transmission",
        Contract: torrentcontract.Contract,
        Name: "Transmission Connector",
        Description: "Controls local Transmission and exposes completed files.",
        ArtifactVersion: "1.0.0",
        Targets: []string{
            connector.PlatformLinuxAMD64, connector.PlatformLinuxARM64,
            connector.PlatformDarwinAMD64, connector.PlatformDarwinARM64,
            connector.PlatformWindowsAMD64, connector.PlatformWindowsARM64,
        },
        Settings: settings,
        SelfTest: func(ctx context.Context) error {
            configured := settings.Get()
            return testTransmission(ctx, &configured)
        },
    })
    runtime.OnStart("transmission", func(ctx context.Context) error {
        return startTransmissionClient(ctx, settings.Get())
    })
    torrentcontract.Add.Handle(runtime, addTorrent)
    torrentcontract.Completed.Handle(runtime, connector.BoundLocalDirectory(
        settings.Directory(func(value *Settings) *string { return &value.CompletedDir }),
    ))
    return runtime
}
```

`connector.New` is definition-only. `Settings.Get` fails during definition and
returns the host-supplied snapshot while runtime execution is active. Settings
are strict JSON: unknown fields, missing required values, invalid typed values,
and failed validation reject readiness. Defaults declared in connector tags are
applied before validation.

Supported setting kinds are `string`, `secret`, `bool`, `integer`, `duration`,
`url`, `file`, `directory`, and `enum`. Options are `required`, `default=`,
`enum=a|b`, `name=`, and `description=`. Secrets use `connector.Secret` and do
not appear in manifests. Settings are flat exported struct fields and cannot use
custom JSON/text marshalers or JSON tag options.

`Runtime.OnStart` executes in registration order after settings and directory
roots are bound. Its context is canceled when the host stops the child. Use it
for process-local clients and goroutines.

## Host protocol

Normal execution uses stdin and stdout exclusively for host protocol frames.
Each frame is a 4-byte big-endian length followed by strict JSON and is bounded
to 8 MiB. Logs go to stderr. The host sends initialization, settings, jobs, and
cancellations; the child sends readiness, progress events, and completions.
Initialization supplies the installation ID, private child state directory,
settings, and approved storage origins.

The child validates exact operation descriptors, deadlines, payload limits, and
cancellation. Mutating commands and directory operations retain durable
idempotency records under the host-provided child state directory. A crash after
an operation starts leaves an explicit indeterminate outcome rather than
silently repeating a side effect.

The binary also accepts `manifest`, `version`, and `validate` for local
inspection. `run` or no argument starts hosted-child transport. Enrollment,
configuration persistence, service installation, start/stop, upgrade, rollback,
and Airlock polling are not connector commands.

## Testing

Use `connectortest.New(t, newConnector)` to invoke the exact definition factory
and validate its frozen manifest:

```go
func TestConnectorDefinition(t *testing.T) {
    env := connectortest.New(t, newConnector)
    if env.Manifest.Interface.Kind != "transmission" { t.Fatal("wrong connector") }
}
```

`connector.LocalDirectory` uses `os.Root` containment, canonical relative paths,
bounded reads and listings, and same-root atomic replacement. Connector handlers
should use direct Go APIs or typed process arguments rather than interpolating
request values into a shell.
