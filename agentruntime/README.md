# Agent Runtime

`Run(ctx, Input) (*Result, error)` runs an application-owned task against injected
models, durable persistence, capability dispatch, and an isolated JavaScript
executor. This package imports neither the SDK root nor Airlock. No database,
authorization, scheduler, process-global coordination, or Docker test dependency
is part of the loop.

## Host Integration

`Input` requires `Definition`, `Model`, explicit `ModelLimits`, `Store`, `Backend`,
`ExecutorFactory`, `Controller`, `Sink`, and `Redactor`. Supply a nonempty `Message`
for a fresh run. Resuming an existing checkpoint ignores `Message`, so the user
message is not appended twice. `Subagents []wire.AgentDefinition` must exactly
cover the slugs in `Definition.Subagents`; these definitions supply the exact
child input schemas. `Capabilities` is the host-authorized JavaScript catalog.
`RecoveryNotice` is optional host recovery context for the next model request.

```go
result, err := agentruntime.Run(ctx, input)
if result != nil && result.CleanupError != nil {
    // Log the teardown diagnostic separately; it cannot invalidate a committed reply.
}
switch {
case errors.Is(err, agentruntime.ErrWaiting):
    // Persist the parked state, release the worker, and schedule from the
    // dependency/deadline that Controller.Wait already recorded.
case err != nil:
    // Settle or recover the attempt according to durable host policy.
case result.Reply != nil:
    // The completed checkpoint is committed. Finalize with result.Reply.
}
```

`ErrWaiting` accompanies `Result{Waiting: true}`. Both `output` and `needs_input`
replies finish the run. A continuation is a **new run checkpoint in the same
session**, not resumption of a completed run. Starting an independent task uses a
fresh session. Reinvoking a completed checkpoint returns its saved reply without
calling the model, controller, or executor.

`Result.CleanupError error` reports executor teardown failures separately from a
durably completed reply. After an acknowledged completed checkpoint commit,
`Run` returns that reply with a nil execution error even if teardown fails; the
host must log `CleanupError` without changing completed status. For waiting,
the cleanup failure is both in `CleanupError` and joined with `ErrWaiting`.
Unfinished runs without a result keep cleanup failures in the returned error
chain alongside the execution error. A failed or unacknowledged checkpoint save
remains an execution error, not proof of completion; recovery loads the durable
checkpoint. Cleanup diagnostics are attempt-local and are not persisted in it.

The host validates the deployment's definition contract and bounds authority to
the admitted run. The checkpoint also verifies its version and `ContractHash` on
resume. Authorization is required again on every controller/backend operation;
model arguments, call IDs, and checkpoint contents never establish authority.

## Store Contract

`Store` embeds `session.SessionStore` and adds `LoadCheckpoint` and
`SaveCheckpoint`. The host scopes it to one logical run and its conversation.

- `LoadCheckpoint` returns `nil` only for a fresh run. Preserve every checkpoint
  field, including the call journal, result messages, reply, and used call IDs.
- `SaveCheckpoint` atomically appends `Checkpoint.Append` to the transcript and
  replaces the checkpoint. No separate runtime call to `Append` is made.
- `Revision` starts at 1 and advances by exactly one. Fence every write against
  the live lease and current revision. An identical committed revision can be
  retried without appending twice; a conflicting/stale revision must fail.
- `Append` is the payload of that transaction, not uncommitted work. A loader may
  return it intact; the runtime clears it before constructing its next commit.
- `Load` returns the current context window. `Compact` atomically replaces that
  window, retains audit history, and enforces the same lease. It runs only at
  model boundaries, independently of journal advancement.
- Persist message IDs and all multipart/provider metadata. Tool attachment data
  contains host-resolved storage references, not JavaScript heap snapshots.
- Keep completed checkpoint outcomes and transcript appends in the same commit.
  A lost commit acknowledgement must be recoverable by loading durable state.

Use durable attempt leases with fencing and serialize session continuations
across replicas. Never use `session.MemoryStore` or a process-local lock as a
production durability or coordination mechanism. Exact dispatch arguments can
contain sensitive data; protect checkpoint access like other execution records.

## Checkpoint Phases

| Phase | Durable Meaning | Recovery |
| --- | --- | --- |
| `ready` | No unresolved model batch | Load/compact history and reserve the next model turn |
| `model` | Model response not committed | Add an interruption notice; no local tool from this response has executed |
| `tools` | Complete model response and ordered call list committed | Dispatch from `Next`; completed results are never regenerated |
| `dispatch` | Current call may have executed | Replay only idempotent admission, wait, or completion; otherwise record an unknown-outcome notice |
| `waiting` | Exact pending wait parked | Invoke the same wait ID and arguments, then the ordered tail without another model turn |
| `completed` | Reply and all transcript outcomes committed | Return the saved reply |

Every call has a predispatch checkpoint and a postresult checkpoint. Interrupted
`run_js`, `get_calls`, and `cancel_calls` are not automatically retried. Their
outcomes become explicit unknown-effect notices, even when the interruption
occurred after the predispatch checkpoint but before the actual dispatch.
Ordinary tool failures are committed as tool errors. A failed JavaScript script
can have completed earlier callbacks; its error warns against repeating the
whole script. Store errors, model errors, context cancellation, and controller
errors implementing `tool.FatalToolError` stop the attempt immediately.

## Controller Contract

- `BeforeModel(ctx)` durably reserves hierarchical reasoning budget before model
  work, including compaction. Its errors terminate the attempt.
- `Spawn(ctx, toolCallID, definitionSlug, input)` returns durable `AgentRunInfo`.
  Deduplicate by parent run plus model tool-call ID, verify matching arguments,
  and recover the same handle after a crash.
- `Continue(ctx, toolCallID, sessionID, prompt)` has the same idempotency rules and
  starts a new child run in a completed session without resetting root budgets.
- `Get(ctx, ids)` inspects owned calls. `Cancel(ctx, ids)` returns cancellation
  information and must apply host ownership and cancellation policy.
- `Wait(ctx, toolCallID, request)` persists selected dependencies and the original
  timeout deadline before returning `ErrWaiting`. Resume is idempotent, including
  repeated parking and timeout expiry. Timeout returns a result without cancelling
  children. Host wakeup/parking must not lose a completion racing with the worker.
- `Complete(ctx, reply)` validates completion policy, rejects active children, and
  accepts an identical replay idempotently. It must not make the checkpoint or
  lease unwritable: run finalization and new session admission occur only after
  the runtime's completed checkpoint commits.

All controller work is scoped by the host, not by IDs supplied by the model. A
rejected `Complete` becomes a tool error, allowing the ordered tail to resolve
children and try completion again. Successful completion suppresses all later
calls and commits explicit skipped outcomes, keeping continuation history valid.

## Models And Budgets

The loop captures one complete goai response with non-executing tool definitions,
then executes the saved calls synchronously itself. `Steps == 0` is unlimited:
there is no Sol runner step default. An explicit budget needs at least one
positive limit; timeout-only and token-only budgets are valid.

The host owns durable step/token totals, descendant aggregation, attempts,
wall-clock deadlines across waits/restarts, and cancellation on expiry or lease
loss. Inject a model wrapper that records all reported input and output tokens,
including compaction and interrupted requests. Tokens are not reserved as a hard
upper bound; an in-flight response can overshoot. The runtime never resets those
durable counters. Session token estimates are only for context compaction.

Step reservation and provider dispatch are separate operations. A crash after
`BeforeModel` commits but before the provider request can consume a step without
model work. Recovery reserves another step; if the interrupted attempt consumed
the last available step, recovery stops at the budget gate without calling the
model. The `model` checkpoint is saved before reservation, so its presence alone
cannot establish that a step was charged. Skipping the gate on that basis would
allow unbudgeted model work. The current controller API provides no durable
per-request reservation key or refund operation.

Compaction reuses `sol/session` overflow detection, pruning, summaries, message
conversion, and continuation prompts. Both compaction and normal model inputs are
redacted. Sink text is emitted after response commitment as a complete redacted
chunk, preventing secret leakage across streaming chunk boundaries. Sinks are
diagnostics, not the authoritative execution journal.

## Tools And JavaScript

Direct model controls are `spawn_<sanitized child slug>`, `continue_agent`,
`get_calls`, `wait_calls`, `cancel_calls`, and `complete`, alongside synchronous
`run_js`. None of those controls is added to JavaScript bindings. The model sees
the child input schema unchanged and a discriminated completion schema containing
the definition's exact output contract or a nonempty `needs_input` question.

Runtime input/output validation supports the SDK's JSON schema vocabulary:
objects, required/additional properties, arrays, primitives and type unions,
local JSON-pointer references and recursive definitions, `enum`/`const`,
`allOf`/`anyOf`/`oneOf`/`not`, numeric bounds/multiples, size constraints,
uniqueness, and RE2 string patterns. Numeric validation preserves JSON precision.
`format` is an annotation. Unsupported assertions, `$id` scopes, remote references,
and malformed schemas fail before model work; no remote schema is fetched.

`chatruntime.JavaScriptTool` reuses the production executor, canonical invoker,
callback attribution, logs, and attachments. Factory allocation is lazy and the
realm is closed on every exit, including waiting and failure. The host passes
only scoped bindings and enforces executor resource limits. Backend results must
already be redacted before entering JavaScript. Application-owned work has no
interactive confirmation flow; host policy authorizes it.

The realm has no ambient network, filesystem, process, or host credentials. Use
`await` for callbacks and explicitly return needed data. JavaScript heap state
is never checkpointed and completed scripts are never replayed to reconstruct
it. Keep durable state in host resources and transcript results.

## Verification

```sh
go test ./agentruntime ./chatruntime
go test -race ./agentruntime ./chatruntime
go vet ./agentruntime ./chatruntime
```

Tests use scripted models, JSON-roundtripped in-memory stores, injected executor
sessions, and simulated precommit/postcommit failures. They require no Docker or
provider credentials. Production uses the existing `jsexec.Session` executor
machinery. Host SQL transactions, leases, authorization, scheduler races, and
actual executor containers require their own integration tests.
