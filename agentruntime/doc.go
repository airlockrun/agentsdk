// Package agentruntime runs application-owned task agents with durable ordered
// tool execution. It has no database, authentication, scheduler, or SDK-root
// dependency. Controls are model tools, never JavaScript capabilities.
//
// A host scopes Store to one run and its conversation, and serializes all run
// attempts and session continuations using durable leases with fencing. Every
// SaveCheckpoint transaction must append Checkpoint.Append and replace the
// checkpoint atomically, rejecting a stale revision or expired lease. An exact
// retry of a committed revision is idempotent. LoadCheckpoint returns nil only
// for a fresh run; continuations use a fresh run checkpoint in the same session.
// Load returns the current compacted transcript. Compact must atomically advance
// the session's context window, retain audit history, and enforce the same lease.
// The runtime does not call Store.Append separately. No in-memory or no-op store
// is suitable for production.
//
// Controller owns authority, child admission, durable budgets, cancellation and
// completion. Spawn and Continue must deduplicate by parent run plus model tool
// call ID and recover the same durable handle after interruption. Wait must
// persist its dependency and original deadline by that key before ErrWaiting,
// and be idempotent on resume. Complete must atomically reject active children
// and accept an identical reply idempotently: it can be called again after a
// crash. Other interrupted calls have unknown effects and are not replayed.
// Complete must leave the lease and checkpoint writable until the runtime's
// completed checkpoint commits. Only then may the host finalize the run and
// admit a session continuation. Ordinary control errors are model feedback;
// errors implementing tool.FatalToolError terminate the attempt. Context errors
// and all store/model/budget errors terminate the attempt without executing tail
// calls. Persisted tool arguments are exact dispatch data and can be sensitive;
// access to checkpoints requires the same protection as execution records.
//
// BeforeModel reserves a durable hierarchical reasoning step before each model
// request, including compaction. The injected model wrapper accounts for all
// input and output tokens, including compaction and interrupted requests. The
// host enforces wall-clock deadlines across waits and restarts and cancels the
// supplied context on expiry or lease loss. Zero Steps means unlimited steps;
// no Sol runner defaults are applied. Host enforcement covers descendants too.
// A reservation is not atomic with model dispatch. An interruption after a
// successful BeforeModel can consume a step without a request; recovery reserves
// another step and can exhaust the remaining budget. PhaseModel alone cannot
// prove whether BeforeModel committed, so it must not bypass the budget gate.
//
// ExecutorFactory is the existing chatruntime factory backed by jsexec in
// production. Each uninterrupted attempt allocates lazily and closes its realm
// on every exit. JavaScript heap state is never persisted or reconstructed by
// replaying completed calls. Models must use transcript results for durable data.
// All dependencies, including Redactor, are required; a host with no secrets
// supplies an explicit identity redactor. Backend must redact callback results
// before they cross into JavaScript. Runtime redacts transcript and sink text.
// Executor close failures after acknowledged completion are returned in
// Result.CleanupError with a nil execution error. The host logs that diagnostic
// without failing the completed run. On unfinished or waiting exits, cleanup
// failures remain in the returned error chain; waiting Results also expose the
// diagnostic separately. Cleanup diagnostics are not checkpoint state.
package agentruntime
