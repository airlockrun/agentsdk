# RegisterExecEndpoint — run commands on a remote target

> Companion to `/libs/agentsdk/llms.md` — read that first. Come here when your task involves running commands on a remote machine over SSH.

Use when the agent needs to run a command on a server its container can't
reach as a built-in tool (managing a VPS via SSH, kicking a CI runner,
restarting a service on a homelab box). Airlock owns the transport
(SSH today) and the credentials; the agent author declares the slug and
wraps calls in domain-specific tools.

```go
ci := agent.RegisterExecEndpoint(&agentsdk.ExecEndpoint{
    Slug:        "ci-runner",
    Description: "Self-hosted GitHub Actions runner",
    LLMHint:     "use `kick-build --branch <name>` to start a build",
    Access:      agentsdk.AccessAdmin, // default; opt down explicitly
})

type KickIn struct {
    Branch string `json:"branch" jsonschema:"description=Branch to build"`
}
type KickOut struct {
    ExitCode int    `json:"exitCode"`
    Stdout   string `json:"stdout"`
}

agent.RegisterTool(&agentsdk.Tool[KickIn, KickOut]{
    Name:        "kick_build",
    Description: "Kick the CI build for a branch.",
    Access:      agentsdk.AccessAdmin,
    Execute: func(ctx context.Context, in KickIn) (KickOut, error) {
        res, err := ci.Run(ctx, agentsdk.ExecCommand{
            Command: "kick-build",
            Args:    []string{"--branch", in.Branch},
            Timeout: 30 * time.Second,
        })
        if err != nil {
            return KickOut{}, err
        }
        return KickOut{ExitCode: res.ExitCode, Stdout: string(res.Stdout)}, nil
    },
})
```

**Configuration is operator-side.** The agent declares the slug,
description, hint, and access level. The operator configures
host/port/user in the Airlock UI; airlock generates an ED25519 keypair,
displays the public key, and the operator pastes it into
`~/.ssh/authorized_keys` on the target. Private keys never leave
airlock and are encrypted at rest. The public key carries a dated
comment (`airlock-{agentSlug}-{endpointSlug}-YYYY-MM-DD`) so on rotation
the operator can `grep` `authorized_keys` and remove old lines.

**Default access is `AccessAdmin`.** Exec hands arbitrary commands to a
real machine; admin-only is the right default. **`AccessPublic` is
silently demoted to `AccessUser`** at registration time with a startup
warning — exec endpoints are never reachable by unauthenticated callers,
period. (The demotion is a friendly recovery, not an error, because
copy-pasting from `RegisterRoute` where Public is meaningful is a
believable mistake.)

**`ExecHandle.Run` is bound into the JS VM as `exec_{slug}.run(command, args?, opts?)`** —
gated at bind time on `Access`. Wrap in typed tools when you want a
narrower verb than "run an arbitrary command":

```javascript
// JS-side (run_js); only available on admin runs by default
exec_ci.run("kick-build", ["--branch", "main"])
// inline (both streams ≤ 8 KiB):
//   { exitCode, durationMs, stdout: "…", stderr: "…" }
// spilled (stdout exceeded 8 KiB):
//   { exitCode, durationMs,
//     stdoutPreview: "…1 KiB head…",
//     stdoutSavedTo: "tmp/exec-ci-a3f9c2b1-stdout.bin",
//     stdoutSize: 50000,
//     stderr: "…",
//     note: "stdout (50000 bytes) exceeded inline threshold; saved to … Use fileRead(stdoutSavedTo) to read." }
```

When `stdoutSavedTo` or `stderrSavedTo` is set, the corresponding
`stdout`/`stderr` field is absent — read the full payload with
`fileRead(savedTo)`. See **Reading overflowed responses** below.

**Shell features (pipes, redirection, env expansion) work** because SSH
hands the full command line to the remote shell. Put pipes in
`Command`; use `Args` for safe multi-arg invocation (args are
POSIX-shell-quoted before being joined onto the remote command):

```go
// Multi-arg with whitespace — args are quoted safely.
vps.Run(ctx, agentsdk.ExecCommand{
    Command: "ls",
    Args:    []string{"-la", "my dir"},
})
// Pipe + JSON parse — everything in Command, no Args.
vps.Run(ctx, agentsdk.ExecCommand{
    Command: "kubectl get pods -o json | jq '.items[] | .metadata.name'",
})
```

**Errors:** `*agentsdk.ExecError` for transport / timeout / config
problems (the command never ran — different retry strategy than a
runtime failure). Non-zero exit codes return a normal `ExecResult` —
inspect `ExitCode` and `Stderr`.

**`Run` is for structured small responses** (JSON API replies, HTML
pages, CLI summaries that fit in your head). Cap is **20 MiB per
stream**; overflow returns `agentsdk.ErrOutputTooLarge` with no
partial result. The run-record audit log keeps an 8 KiB preview per
stream for visibility in the runs UI; the caller of `Run` still
receives the full data up to the 20 MiB cap.

**`RunStream` is the default for any actual data download.** Tarballs,
log archives, database dumps — anything you'd describe as "data"
rather than "a result." Returns an `*ExecStream` whose `Stdout`/`Stderr`
are `io.ReadCloser`s and `Wait()` blocks for the exit code:

```go
s, err := vps.RunStream(ctx, agentsdk.ExecCommand{
    Command: "tar -czf - /var/log",
})
if err != nil { return err }
defer s.Stdout.Close()
defer s.Stderr.Close()

// Pipe straight into agent storage — zero buffering in agent RAM.
info, _ := agent.WriteFile(ctx, "tmp/logs.tar.gz", s.Stdout, "application/gzip")
exit, _ := s.Wait()
if exit.ExitCode != 0 {
    stderrBytes, _ := io.ReadAll(s.Stderr)
    return fmt.Errorf("tar failed: %s", stderrBytes)
}
return nil
```

`RunStream` is **Go-only** — there is no JS binding for it. The
JS-facing `exec_{slug}.run` streams under the hood and auto-spills
stdout/stderr above 8 KiB to `tmp/exec-{slug}-{callID}-stdout.bin` /
`-stderr.bin` (both halves of a call share `callID` so they correlate).

**Rule of thumb:** if the downstream code is going to call `JSON.parse`
on the result, use `Run`. If it's going to call `agent.WriteFile`, use
`RunStream` and skip the round-trip through agent RAM.

### Reading overflowed responses

`conn_{slug}.request`, `conn_{slug}.requestJSON`, `exec_{slug}.run`, and
`httpRequest` all share the same overflow shape: any payload above 8 KiB
is saved to a `tmp/...` storage path, with `*Preview` holding the first
~1 KiB and `*SavedTo` holding the path. Inline and spilled keys are
mutually exclusive — check the saved-to field and branch:

```javascript
// Connection
const r = conn_x.request("GET", "/big")
const body = r.bodySavedTo ? fileRead(r.bodySavedTo) : r.body

// Connection (JSON)
const r = conn_x.requestJSON("GET", "/big.json")
const data = r.bodySavedTo ? JSON.parse(fileRead(r.bodySavedTo)) : r.data

// Exec
const r = exec_x.run("dump-state")
const stdout = r.stdoutSavedTo ? fileRead(r.stdoutSavedTo) : r.stdout
```

Don't re-read an older `savedTo` after a fresh call to the same binding
— each call gets a unique `callID` so paths don't collide within a run,
but holding onto a stale path across multiple LLM steps is a bug source
the `note` field warns about.

