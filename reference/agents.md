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
})
research := agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[ResearchInput, ResearchOutput]{
    Slug: "researcher",
    Description: "Research a topic and return a concise summary",
    Instructions: "Research the topic using the source. Submit a factual summary. Ask a question when required information is missing.",
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
arrays. Numeric schemas advertise integer or number types without inferred Go
machine-width limits. Explicit `minimum` and `maximum` field tags are preserved,
including exact integer JSON values. Array lengths and explicit annotations
participate in the contract hash; numeric width alone does not change the schema.
Floating-point values retain Go's rounding semantics. The SDK checks array lengths
and native numeric ranges before decoding a typed reply, even without advertised
numeric bounds.

No-auth connections and MCP servers use their fixed declared endpoint directly
through Airlock's outbound transport and do not create or bind a user resource.
Their `BindingMode` is optional and ignored; `shared` and `per_user` remain
accepted so existing declarations compile unchanged. Connection static headers
belong to the trusted app declaration and are shared by every invocation of
that app endpoint, so they must not contain per-user credentials or imply
account isolation. Authenticated declarations require an explicit `BindingMode`
and use the corresponding shared or per-user resource binding.

`Tools []tool.Tool` accepts native goai tools, normally built with
`tool.Typed[In, Out](name).Description(...).Execute(fn).Build()`. They require
an executor and input/output schemas. These tools are private to the definition,
not added to `RegisterTool` or another definition. Two definitions can each have
a tool of the same name with independent implementations. A scoped invocation
does not fall back to global tools. Tool descriptions, schemas, and input examples
form part of the persisted contract. The task-agent TypeScript declarations bind
them as `tools.<name>` inside `run_js`; this private namespace is not exposed to
web, bridge, or global MCP conversations.

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

### Generic operation allowlist

`Capabilities *agentsdk.AgentCapabilityPolicy` selects generic operations by
canonical ID. Use typed SDK constants for built-ins:

```go
Capabilities: &agentsdk.AgentCapabilityPolicy{
    Allow: []agentsdk.AgentCapability{
        agentsdk.CapabilityAttachToContext,
        agentsdk.CapabilityFileStat,
        agentsdk.CapabilityFileRead,
        agentsdk.CapabilityFileWrite,
    },
},
```

Nil preserves the normal generic inventory. `&agentsdk.AgentCapabilityPolicy{}`
permits no broker-dispatched generic operations. Private `Tools` and explicitly
bound `MCPs` remain selected by those declarations. `run_js`, completion/child
controls and executor-local console/`air.log` are runtime intrinsics, not grants
to broker operations. Human conversations do not use a task definition's policy.

An explicit policy also excludes unlisted connection and topic operations.
Select exact canonical IDs using the existing path API, for example:

```go
agentsdk.AgentCapability(capability.Local(capability.Connection, "source", "request").ID())
```

The connection/topic must be declared on this app. JS names (`air.fileRead`,
`conn.source.request`) and provider aliases are not IDs. Unknown entries,
duplicates, private-tool/MCP entries and undeclared resources fail registration.
`CapabilityAttachToContext` requires explicit `CapabilityFileStat`: local image
adapters use authorized metadata lookup and never silently expand the allowlist.
File operations still enforce directory, scope and caller policies; selecting
an operation grants no home-file, foreign-app or unregistered-directory access.

Registration snapshots and sorts the list. Policy presence and entries enter
the contract hash, so an unset and an explicit empty policy are distinct through
sync, serialization and recovery. The shared catalogue filters operations before
JS/direct exposure and on SDK, local-host and hosted broker dispatch. Direct
calls to forbidden canonical IDs or presentation aliases cannot bypass it.
An explicitly restricted parent requires each child to have an explicit policy
whose generic operations and MCP selection are subsets of its own. Private
child tools remain owned by the explicitly declared child definition. Existing
live host authority and resource-binding checks remain independent gates.
Trusted native Go tools retain their own app APIs; this is a model/callback
capability restriction, not a native-code sandbox.

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

### Original admission input in private callbacks

`handle.Input(ctx) (In, error)` returns a freshly decoded copy of the original
session-start input. Only an authenticated private or review callback for that handle's
exact app, run, definition and contract can read it. It performs no network call
and needs no app task-data table. Tool arguments and continuation prompts cannot
replace the snapshot; every child session has its own original admission.
Missing/foreign callback state, invalid input and cancelled contexts return
errors. Mutating the returned struct or its nested slices does not mutate the
stored snapshot. Paths in input identify storage; immutable paths or checked
content fences are still needed to freeze source bytes.

```go
var extraction *agentsdk.AgentHandle[Input, Output]
service := NewService(app, func(ctx context.Context) (Input, error) {
    return extraction.Input(ctx)
})
extraction = Register(app, service)
```

The host supplies this input from persisted original admission under the current
run/definition/owner/receipt fences. Localruntime and agenttest use the same
delivery contract. Human routes/global tools have no task-input context.
Saved-script execution is described in `reference/scripts.md`.

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
`AgentRunSubmitted`, `AgentRunReviewing`,
`AgentRunCompleted`, `AgentRunFailed`, `AgentRunCancelled`, and
`AgentRunBudgetExceeded`. `Status.Terminal()` reports completion, failure,
cancellation, or budget exhaustion. Waiting means durable child dependencies;
it is not a request for human input.
Submitted means durable evaluation is pending; reviewing means a review delivery
is active. Both are nonterminal and expose no `Reply`. `Wait` observes the entire
review/correction lifecycle.

A completed run has `*AgentReply[Out]` with exactly one kind:

- `AgentReplyOutput`: `Output *Out` contains the accepted typed result; `Question` is empty.
  `Artifacts []AgentArtifact` contains the host-resolved immutable manifest.
- `AgentReplyNeedsInput`: `Question` is nonblank; `Output` is absent.

Both reply kinds finish the run as `AgentRunCompleted`. Any completed session
can receive a `Continue` prompt, including one that produced output. There is no
yield status or special input suspension.

## Typed submission review

The model finishes a batch with `submit`. It accepts
`{kind:"output",output:<typed result>,artifacts:[{path:"reports/result.json"}]}`
or `{kind:"needs_input",question:"..."}`. Artifacts are optional, bounded to 32,
and may carry expected `sha256` (lowercase hex) or `contentId` assertions. The
model cannot supply entry IDs, sizes or content types as authority. The host
resolves and pins actual metadata under the subject run's storage permissions.
Successful submit denies every remaining call in the response.

Add an optional typed reviewer to a definition:

```go
Review: &agentsdk.AgentReview[ResearchInput, ResearchOutput]{
    Version: 1, Timeout: 30 * time.Second,
    MaxAttempts: 3, MaxConcurrency: 2, MaxCorrections: 2,
    Handler: func(ctx context.Context, review agentsdk.AgentReviewContext,
        s agentsdk.AgentSubmission[ResearchInput, ResearchOutput],
    ) (agentsdk.AgentReviewDecision, error) {
        if s.Output.Summary == "" {
            return agentsdk.Continue("Include a factual summary."), nil
        }
        return agentsdk.Accept(), nil
    },
},
```

Version, delivery attempts and concurrency are positive. Timeout is a positive
whole-millisecond duration bounded to 24 hours. Delivery attempts are bounded to
100, concurrency to 1000. MaxCorrections is nonnegative; zero allows no automatic
correction. Registration snapshots the policy and callback and derives an
internal versioned Go job. Applications do not register/enqueue that job themselves.
The review policy participates in the definition hash.

`AgentSubmission[In, Out]` exposes ID, RunID, SessionID, ToolCallID, ContractHash,
Revision, Input, Output, Artifacts and Decision. Pending callbacks have no Decision.
Input is a fresh copy of the original admitted Start input, including on
corrections. Output is the typed draft for this revision. `AgentReviewContext`
exposes the submission ID, subject RunID/SessionID/Revision and Job attempt
diagnostics. The review execution run is distinct from the subject run.

Use `review.Open(ctx, s.Artifacts[i])` to read exact pinned bytes. It returns an
`io.ReadCloser` the reviewer must close. It rejects foreign or mutated manifest
entries and requires the exact callback execution/job/attempt/lease context.
Replacing or deleting a display path does not change the submitted content.
Ordinary storage reads select current content and do not provide this guarantee.
Original `handle.Input(ctx)` access remains scoped to the matching definition.

Return `Accept()` to publish this output, `Fail(reason)` to terminate the subject,
or `Continue(feedback)` to request correction. Fail/Continue require nonblank
feedback of at most 4096 bytes. A returned error is review execution failure.
Corrections preserve the subject run ID, original input, captured call IDs and
cumulative reasoning budgets. They append durable feedback and resume the model
with a fresh JavaScript realm, without replaying interrupted effects. Public
`handle.Continue` creates a new run in a completed session; it cannot continue a
submitted/reviewing session. needs_input bypasses review and completes a question.
Omitting Review atomically autoaccepts output during the submission commit.

Review delivery is at least once. Deduplicate any external effects by submission
ID; the persisted decision and feedback transition do not imply exactly-once Go
or external effects. Host scheduling and decision settlement retain live authority,
deadline/cancellation fences and exact admitted executable identity throughout
review and correction. Callback code is not identified by declaration hash alone.
Hosts retain submission drafts and pinned content without an arbitrary SDK expiry.

Checkpoint version 2 distinguishes a submitted draft from completed questions.
Controller.Complete validates only; SaveCheckpoint commits the submission,
artifact pins, ordered transcript and review scheduling/autoaccept atomically.
Recovery of a committed submitted checkpoint does not call the model again.
Version 1 completed checkpoints preserve their recorded replies; in-flight
version 1 checkpoints require draining or explicit termination before cutover.

Sessions persist conversation messages and compaction. Interrupted generic tool
calls with uncertain outcomes produce an explicit recovery notice; side effects
are not automatically replayed or externally reconciled. Native tools should use
domain-specific idempotency when their own business operation requires it.

## Tests

Local tests cover typed domain logic, explicit GoAI model responses, schemas,
checkpoint algorithms and offline factory manifests. Client protocol fixtures
validate serialization/error handling without claiming host execution semantics.

Registered tasks, review/artifact submission, durable scheduling, cancellation,
recovery and hierarchical budgets execute on the real Airlock host. Candidate
integration tests use `remote.TestClientFromEnvironment()` with the host's scoped
capability and exact revision/runtime coordinates; there is no simulated Env or
task scheduler. Model policy belongs to the selected real target, never an implicit
laptop account. See [testing](testing.md) for local/remote suite commands.
