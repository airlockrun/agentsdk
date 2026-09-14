# Task Agents

`RegisterAgent[In, Out]` declares an application-owned task executed by Airlock.
The app supplies a typed contract, private Go tools, a text model slot, optional
MCP bindings, and optional leaf subagents. Airlock owns the model loop, durable
sessions, scheduling, recovery, and hierarchical budgets. Definitions are not
ordinary chat tools. Expose a task through an app route or registered tool only
when that interface's authorization and product behavior call for it.

## Registration

```go
type ResearchInput struct {
    Topic string `json:"topic"`
}
type ResearchOutput struct {
    Summary string `json:"summary"`
}

a.RegisterModel(&agentsdk.ModelSlot{
    Slug: "research", Capability: agentsdk.CapText,
    Description: "Task research and synthesis",
})
source := a.RegisterMCP(&agentsdk.MCP{
    Slug: "source", Name: "Research source", URL: "https://example.com/mcp",
    AuthMode: agentsdk.MCPAuthNone,
    // Access is omitted: no ordinary chat exposure.
})
research := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[ResearchInput, ResearchOutput]{
    Slug: "researcher",
    Description: "Research a topic and return a concise summary",
    Instructions: "Research the topic using the source. Complete with a factual summary. Ask a question when required information is missing.",
    ModelSlot: "research",
    MCPs: []*agentsdk.MCPHandle{source},
    MaxAttempts: 3,
    MaxConcurrency: 4,
})
```

Register the model, MCP servers, and children before the definition. `ModelSlot`
must name a registered `CapText` slot. `MaxAttempts` and `MaxConcurrency` are
required positive integers. Slugs are lowercase snake_case, at most 58 bytes.
Reflected input/output contracts support structs, primitives, arrays, slices,
and pointers. Recursive types, maps, interfaces, custom JSON/text codecs, byte
slices, embedded fields, and unsupported JSON tags fail at registration rather
than advertise a schema that differs from their encoding.
Fixed arrays require exactly their Go length, including nested and zero-length
arrays. Numeric schemas carry the declared Go width's bounds, preserving exact
64-bit integer limits. Pointer and nested field constraints are retained. These
constraints participate in the contract hash; changing an array length or numeric
width changes the contract. Floating-point values retain Go's rounding semantics.
The SDK checks array lengths and numeric ranges before decoding a typed reply.

`Tools []tool.Tool` accepts native goai tools, normally built with
`tool.Typed[In, Out](name).Description(...).Execute(fn).Build()`. They require
an executor and input/output schemas. These tools are private to the definition,
not added to `RegisterTool` or another definition. Two definitions can each have
a tool of the same name with independent implementations. A scoped invocation
does not fall back to global tools. Tool descriptions, schemas, and input examples
form part of the persisted contract.

`MCPs []*agentsdk.MCPHandle` binds only explicitly selected servers. Empty
`MCP.Access` means no ordinary chat exposure; it does not disable native calls
or bound task use. Existing MCP authentication options remain available. This
does not introduce a provider-specific or server-credential mode.

`Subagents []agentsdk.Subagent` accepts registered `AgentHandle` values from the
same app. Children cannot have children. A definition with children must set both
`MaxSubagentCalls` and `MaxConcurrentSubagents` to positive integers; without
children these fields must be zero. The runtime exposes explicit child controls
alongside synchronous `run_js`, not as JavaScript globals. Children receive their
exact typed input contracts, and the parent must finish its children before
completing. There is no sibling messaging or app-to-app delegation.

Registration clones schemas, tools, examples, budget values, and reference lists.
`Manifest()` freezes registration and returns an independent, deterministically
ordered declaration. `Slug()` and `ContractHash()` identify the handle's immutable
contract. Changing a declaration changes its hash; persisted sessions require an
exact contract match, and incompatible deployments fail explicitly.

## Budgets

`Budget *agentsdk.AgentBudget` distinguishes omission from explicit limits:

```go
Budget: &agentsdk.AgentBudget{
    Steps: 100,
    Timeout: 30 * time.Minute,
    Tokens: 50000,
},
```

Omission selects only `Steps: 150`. Zero disables an individual limit; an
explicit all-zero budget and negative limits are invalid. `Timeout` is a
`time.Duration` and must be an exact whole number of milliseconds; positive
submillisecond or fractional-millisecond durations fail registration rather than
rounding. The largest accepted timeout is
`time.Duration(math.MaxInt64).Truncate(time.Millisecond)`; conversion to wire
`timeoutMs` preserves its value without overflow. Any enabled limit can
terminate the task with `AgentRunBudgetExceeded`. Steps count model turns,
including descendant reasoning. Tokens accumulate input and output, including
descendants and compaction. Timeout is wall clock including waits and restarts.
In-flight work can overshoot a token limit. Counters persist across recovery;
child continuation retains the root budget. Independent top-level starts get
fresh budgets. `MaxAttempts` bounds interruption recovery, not the number of
completed-session continuations.

## Durable IDs

```go
run, err := research.Start(ctx, requestID, ResearchInput{Topic: "Soil health"})
run, err = research.Get(ctx, runID)
page, err := research.List(ctx, agentsdk.ListAgentRunsOptions{
    Limit: 50, Cursor: cursor, SessionID: sessionID,
    Status: agentsdk.AgentRunCompleted,
})
run, err = research.Wait(ctx, runID)
run, err = research.Cancel(ctx, runID)
run, err = research.Continue(ctx, sessionID, continuationRequestID, "Focus on dry climates")
```

`Start` always creates a fresh session. Persist a canonical non-nil UUID
`requestID` before sending; retry with the same request ID and identical input
after an uncertain response. `Created` distinguishes new acceptance from the
existing idempotent result. `Continue` uses its own persisted request UUID and
creates a new run in the selected completed session. Persist returned run/session
IDs, not in-memory call objects. Reconstruct the same registered handle after
restart and use `Get`, `List`, `Wait`, `Cancel`, or `Continue` with those IDs.

Native starts are application-owned even when `ctx` comes from a human request.
The source human is not substituted as the task's user or initiator. Calls use
the SDK client's app bearer credential. Read APIs and waiting do not mint rolling
background execution runs. A caller context supplies cancellation, not authority
to fabricate a human task. The host authorizes all lifecycle routes.

`Wait` polls `Get` until terminal, using the caller's context. Context cancellation
or a deadline stops observation without cancelling the task. Use `Cancel`
explicitly to stop work. `List` returns `AgentRunPage[Out]` with `Runs` and
`NextCursor`. Typed `List` sends the handle's current `contractHash` on every page;
the host filters by that hash before pagination. It does not return history from
other contracts under the same definition slug. Responses still require an exact
hash match, including when querying by ID. Terminal task failures are state, not
HTTP errors; inspect `Status`
and `Error`. Transport, malformed response, and contract mismatch errors return
through Go's error result.

## Replies

`AgentRunInfo[Out]` exposes `ID`, `SessionID`, `Definition`, `ContractHash`,
`Status`, `Error`, `Reply`, `Steps`, `Tokens`, `CreatedAt`, `UpdatedAt`,
`StartedAt`, `CompletedAt`, and `Created`. Timestamp pointers are absent until
the corresponding event occurs.

Statuses are `AgentRunQueued`, `AgentRunRunning`, `AgentRunWaiting`,
`AgentRunCompleted`, `AgentRunFailed`, `AgentRunCancelled`, and
`AgentRunBudgetExceeded`. `Status.Terminal()` reports completion, failure,
cancellation, or budget exhaustion. Waiting means durable child dependencies;
it is not a request for human input.

A completed run has `*AgentReply[Out]` with exactly one kind:

- `AgentReplyOutput`: `Output *Out` contains the typed result; `Question` is empty.
- `AgentReplyNeedsInput`: `Question` is nonblank; `Output` is absent.

Both reply kinds finish the run as `AgentRunCompleted`. Any completed session
can receive a `Continue` prompt, including one that produced output. There is no
yield status or special input suspension.

Sessions persist conversation messages and compaction. Interrupted generic tool
calls with uncertain outcomes produce an explicit recovery notice; side effects
are not automatically replayed or externally reconciled. Native tools should use
domain-specific idempotency when their own business operation requires it.

## Tests

Use `agenttest.New(t, factory)` to exercise registration, sync, app callbacks, and
the typed lifecycle with a started SDK. `Env.Airlock.SetAgentResponse(method,
requestURI, status, response)` configures an exact lifecycle route, including its
query string. Supply `wire.AgentRunResponse` or `wire.ListAgentRunsResponse`;
unconfigured task responses fail explicitly rather than inventing successful work.
`Requests()` includes cloned request headers for credential assertions.

The mock does not execute the hosted model loop or emulate durable scheduling.
Configure queued, completed, failure, and idempotent responses explicitly. Tests
for host persistence, leases, recovery, and hierarchical budget enforcement belong
to the host and runtime packages.
