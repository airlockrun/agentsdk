# CLI output and observation

Use `go tool air help`, `go tool air help COMMAND SUBCOMMAND`, or
`go tool air COMMAND --help`. Help and version need no login or network.
`--json` on help returns the public command inventory, usage, supported flags
with `name`, `description`, `takesValue`, `required`, optional `valueSyntax`,
`default` and conditional `requiredWhen`, and side effects. Human command help
lists one option per line, including launch/wait behavior and force risks. The same
declarations validate public flags before authentication or network work.
Discovery describes operations; it never grants execution authority.
Internal worker protocols are not part of public discovery.
Value options accept `--flag value` and `--flag=value`; arguments after `--`
are payloads and are not normalized. Boolean switches take no value.
Put subcommands immediately after their parent. Use explicit relative paths
such as `./status` when a directory name matches a subcommand.

## Version 1 result document

`resources list` discovers global resources of all types, including connector
readiness, host/artifact metadata and capabilities. `resources list preview`
lists declared app slots instead; `resources inspect /NAME` or `/UUID` reads a
safe resource projection and `resources inspect preview/SLOT` reads a slot.
Use `resources candidates preview/SLOT --type connector` before selecting a binding.
`connectors install/update` manages installations; `connectors inspect ID`
provides installation diagnostics. Binding/use is independent of management.
Installation inspection includes `data.latestManagementJob` only after independent
host authorization and filters out jobs for other connectors. If that optional
host read fails, `data.managementDiagnosticsUnavailable:true` makes the missing
observation explicit; installation metadata is not a management-job success claim.
An update changes installed executable/settings and must retain its management app/need
and satisfy bound interfaces. A multi-target need can bind a target group containing
several installations; each installation is reserved to one need.
Observe/recover with `hosts job/request/cancel`.
Hosted exact-host management uses the existing one-hour human confirmation;
`approval_required` (77) preserves `error.approvalId`, marks admission blocked and
directs the user to the pending confirmation in the developer conversation.
Do not replay the whole shell. Capture complete stdout separately from stderr and
check process outcome before JSON parsing; truncating or merging streams loses this contract.

Every public command accepts `--json` before the payload separator. Stdout
contains one JSON document. Diagnostics, progress and local subprocess logs
use stderr; there is no implicit JSON Lines or streaming JSON mode.

```json
{"schemaVersion":1,"command":"status","status":"completed","data":{"remote":"work","target":"production","deployment":{"status":"active","running":true}}}
```

Required fields are `schemaVersion`, `command`, `status`, and `data`.
`context` contains selected app/remote/target/runtime coordinates when resolved.
`operation` retains the admitted request/task/revision or test-run identity even
when later observation fails. Its typed `kind` identifies the operation and
`admission` is `preparing`, `unknown`, `accepted`, `blocked`, or `rejected`.
An unknown admission is not an execution failure or permission to retry.
Source uploads retain their archive state and selected runtime even when a
transport error leaves no confirmed task ID; no task ID is fabricated.
`configuration` reports declaration availability
and actual unset names without secret values. Optional `error` contains `code`,
`message`, numeric `exitCode`, and an optional server-owned `approvalId`.
`effect_unknown` preserves recovery coordinates without claiming the mutation
failed. `observation_timeout` means observation stopped, not cancellation;
`command_timeout` identifies other bounded command timeouts.
`guidance` contains a few suggested actions:
`kind`, `message`, `executable`, and an `args` array. Execute that argument array
directly; do not interpolate it into shell text. Human commands quote shell
metacharacters using POSIX shell syntax. Observation actions pin the concrete
selected deployment and use `go -C DIR tool air` when a selected repository is
outside the current directory. Human hints use the saved remote's app binding
without an app UUID when it matches the selected app/origin. They use `--target
preview` only when that app's saved preview reference matches the validated
runtime/name; otherwise an explicit selector remains necessary. These are local
contextual hints, not a replacement for machine identity-pinned actions. Local
help actions use `go -C DIR tool air` in a module declaring the air tool, or the
verified persistent invoking executable outside that context. A transient Go
build executable without a reusable launcher produces no executable help action.
Optional `warnings` carry `code` and `message` for diagnostic inspection failures;
an unavailable runtime inspection does not turn a last-known snapshot into a
current health claim. A denied observation after acceptance retains
`operation.admission:"accepted"` and its task/job/request coordinates;
`observation_permission_denied` and `observation_authentication_required` identify
that denial. Only a verified failed mutation admission can mark admission blocked
or rejected. A denied independent/preflight read cannot establish admission denial.

`status` describes the command outcome, not an arbitrary nested runtime:

- `completed`: the command/observation completed. A snapshot can describe an
  unfinished or failed operation without claiming successful execution.
- `accepted`: an asynchronous mutation was admitted; readiness is not established.
- `pending`: login or server-owned approval requires human action.
- `failed`: command execution/validation failed; inspect `error` and any snapshot.
- `timed_out`: a bounded command/observation stopped; remote work can continue.

Exit 0 means successful command completion or asynchronous admission, not
necessarily readiness. Errors exit 1; server-owned approval exits 77; a completed
managed host shell forwards its exact valid child exit code. Snapshot reads of
failed deployment/build/test state exit 0 and expose the recorded state in data.
Waiting for failed work exits nonzero; cancellation acknowledgment does not
assert rollback. Interrupting observation is
not cancellation. Never resubmit automatically after uncertain mutation effects.
Keep request identity and exact payload; use the supplied recovery action.

`status` data contains `deployment`, `remote`, `target`, and an optional
`pendingTaskId`. `inspect` data contains deployment diagnostic fields. Build data
contains `kind`, `build`, `builds`, and `task` (inactive variants are null), plus the exact
server `routeBaseUrl` and `workspaceUrl`. The command envelope stays the same
when a task progresses to its build. Submission `operation` retains `taskId`,
`revisionId`, `generation` and selected runtime. Submission data separately
captures the last observed serving snapshot. A failed update
can leave an older runtime running; inspect attempted update and serving state
separately. A URL proves only assigned routing, not readiness.

Local build/test output is not buffered in the result document: it goes directly
to stderr. The final `data` records the validation outcome and directory.
Build artifacts are temporary and not retained. Use a caller-owned stderr log
file if a durable local log is required; treat application-generated logs as
potentially sensitive.

Tool invocation data contains `runId` and decoded JSON at `result`. Human output
renders that result directly rather than printing a JSON-encoded string or its
routine completed-run metadata. Application-owned payload IDs remain intact.
Pending agent runs retain actionable run handles. Normal status hides healthy or
unknown environment diagnostics; restart/rebuild/next-start actions remain visible,
and `inspect`/JSON retain the detailed configuration state. Agent,
resource, tool and test inventories use human summaries/tables by default;
explicit inspectors and JSON retain diagnostic identities and schemas.
Environment writes acknowledge the saved value/presence without returning secret
values or guessing runtime activation. Use the selected target's inspection
action for desired/applied environment revisions and restart requirements.

## Payload commands

`request`, `resources request`, and `logs` preserve raw default stdout.
Explicit JSON wraps the body in `data` with `statusCode`, `contentType` when
known, `encoding` (`utf-8` or `base64`), and `body`. Decode according to encoding,
not content type. JSON HTTP/log bodies are limited to 16 MiB; use raw output for
larger responses. HTTP failure keeps its response data and adds an error.
Managed host JSON retains `job` and `events`; `stdout`, `stderr`, and `exitCode`
are null until shell output is available. Decoded streams use the same explicit
UTF-8/base64 encoding contract. No credentials or secret environment values are included
by CLI configuration results or suggested actions.

## Wait without replay

Humans, external agents and hosted agents can use `status --wait --timeout 10m` for admitted
update plus active/running-image readiness, or `deploy status --wait --timeout
10m` for one pinned build. `agent wait DEFINITION RUN --timeout 2m` observes an
existing run. Connector management defaults to bounded verified-inventory waiting;
`--no-wait` only submits. Host commands have server execution deadlines and
bounded observation; cancellation is an explicit separate operation.

Status snapshots of queued/running updates or pending candidates offer contextual
wait guidance, even when an older image is running.
Cached task handles require nonterminal task verification before generating that
guidance. Ready/stopped snapshots and completed waits do not offer another wait;
failed updates offer build diagnostics. Timeouts retain their last snapshot and
admitted task handle, with safe resume-observation/inspection guidance. Output and
guidance depend on the command and observed state, not the caller's hosted context.
These status actions do not change inspect or lifecycle command guidance.

`start --wait` and `restart --wait` observe the current serving runtime, not the
outcome of a separate deployment update. The result reports update state
independently. Observing another caller's server-side task does not rebase the
checkout's cached source CAS state. Reconciliation of this checkout's own admitted
task retains its exact-build, runtime and canonical source-revision checks.
`status --revision UUID --wait` observes an exact serving canonical revision's
runtime without reconciling an unrelated cached task. `inspect --revision UUID`
guards a snapshot the same way. These guards fail on a source mismatch; they do
not wait for a pending candidate UUID to become canonical. An unchanged accepted
upload can converge while a historical build failure remains visible. A newly
built upload's candidate revision can differ from its prepared canonical serving
revision; task/build provenance establishes that relationship.

Use `preview -m "Describe changes" --launch --wait --json` for submission plus
bounded readiness observation. Built-in waits work in hosted foreground tools or
as the single background workspace command. Fit CLI timeout to the tool/caller
deadline; a tool deadline can stop observation before the CLI's default 10m.
Use these waits rather than hand-written status/sleep loops. A HOST completion
may also arrive: match its deployment task UUID to `operation.taskId` (not a
shell request/checkpoint ID), and treat an already observed outcome as information,
not a new request or permission to resubmit. Accepted empty task IDs report no new
build; bounded runtime observation uses the returned canonical revision instead.
Unknown admission requires inspection without replay or an invented task ID.
Hosted identity/default selection and authorization restrictions remain mandatory.

Global resource discovery/use needs no app deployment. Target configuration
uses known target declarations. An inspected pending candidate can expose its
own declarations before activation; a running target can retain the previous
revision's declarations. Unavailable candidate metadata is unknown availability,
not evidence of missing configuration. Actual missing names and inspection
failures remain visible. Settings links use the server-provided browser workspace
URL and supported settings routes, never the internal API origin.
