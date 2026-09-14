# Chat Runtime

`Run(ctx, Input) (*sol.RunResult, error)` is the shared hosted-chat entrypoint.
This package imports neither the SDK root nor Airlock. The host owns run creation,
completion, durable permission checkpoints, and conversation serialization.

Required input:

- `Model stream.Model` and explicit `ModelLimits session.ModelLimits`.
- `SessionStore session.SessionStore`, scoped to the conversation.
- `MaxSteps`, a positive run budget.
- `Sink eventstream.Sink`, receiving Sol text, tool, permission, compaction, and
  suspension events. Completion is represented by the returned `sol.RunResult`.
- `Backend`, implementing `Invoke(context.Context, Invocation) (tool.Result, error)`.
- `Capabilities []capability.Definition`, the authorized subset of the canonical
  catalog. Authorization remains mandatory in the backend on every invocation.
- `ExecutorFactory` for JS mode. Direct mode never allocates an executor.

`Invocation` contains `CapabilityID`, `ToolCallID`, and JSON `Input`. The backend
binds run/user/job attribution independently of script input. App capabilities
use `wire.RuntimeInvokePath`; platform capabilities execute in the host broker.
Backend output must be redacted before crossing the JS/model/event boundaries.
Attachments carry storage references resolved by the host model adapter.
`Definition.TextOutput` distinguishes plain-text file results from encoded JSON;
the invoker preserves their string type even for JSON-looking file contents.

`ExecutorFactory` is `func(context.Context, []capability.Definition)
(jsexec.Session, error)`. Bind each non-executor definition using its canonical
`Path.ID()` as `jsexec.Binding.Name` and `Path.JSParts()` as the JS path. Each JS
function takes **one object argument** matching `Definition.InputSchema`;
the invoker receives a positional JSON array containing that object. Console
methods and `air.log` are executor intrinsics, not backend invocations.
The host factory supplies optional `jsexec.Options.User` from its authenticated
run scope. Only `id`, `email`, and `displayName` enter the realm; no grants or
credentials do. JavaScript receives a frozen lexical `user` object, or `null`.
For local tests, set `agenttest.ExecutorConfig.User` to the same identity as
`Env.Chat`'s `wire.RuntimeContext`. This display context never authorizes calls.

JS executes as an async function body: `return await tools.lookup({id: "123"});`.
Model tool calls are serial. A script may await bounded concurrent callbacks.
Explicit `globalThis` data can persist across scripts in one uninterrupted run;
it does not survive suspension, completion, cancellation, or replica movement.
The whole `run_js` approval gate runs before lazy executor allocation. `Run`
closes its executor on all exits. Execution and callback limits are enforced by
the supplied `jsexec.Session` outside the untrusted realm.

`Instructions` is host-composed prompt text. `RenderPrompt` and `RenderTypeScript`
use the same catalog as execution, including canonical schemas and examples.
Both return `(string, error)`; `Run` rejects schema-rendering errors before model
execution. Raw MCP schemas support unions, local acyclic `$ref`s, and typed
`additionalProperties`. Unrestricted schemas render as `unknown`; unsupported
references and malformed type-bearing keywords fail explicitly. TypeScript is
a description, not a replacement for the backend's JSON Schema validation.
`DirectTools` selects individual model tools instead of `run_js`. `AutoConfirm`
is an explicit host policy for noninteractive runs.

Resume uses `Resume *sol.SuspensionContext` and required `Approved *bool`.
Only permission suspension is accepted. The host must reject incompatible
checkpoints before constructing Input. External MCP servers declared in the
manifest are ordinary broker capabilities.

`JavaScriptTool(ctx, catalog, backend, factory)` exposes the same synchronous
executor/invoker machinery to application-owned task loops such as
`agentruntime`. It returns the tool, a required close function, and an error.
Allocation is lazy; callers close on completion, parking, cancellation, and
failure. This helper has no interactive approval flow: the host preauthorizes the
catalog and enforces policy in the backend. It adds no agent-control bindings.
