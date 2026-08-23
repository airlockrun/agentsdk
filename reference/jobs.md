# Durable jobs

Jobs are typed, versioned contracts with durable lifecycle state and at-least-once
delivery. Input must be a struct. Use `JobContext.ID` as the idempotency key.
`JobContext.ScheduledAt` contains the intended occurrence time for cron and
delayed jobs and is nil for immediate jobs.

Handlers can synchronously persist a progress snapshot for their active delivery:

```go
err := job.ReportProgress(ctx, agentsdk.JobProgress{
    Phase:     "rendering",
    Message:   "Rendered 40 of 100 pages",
    Completed: 40,
    Total:     100,
})
```

`Phase` must be nonblank after trimming and at most 128 bytes. `Message` is at
most 4096 bytes. Counts are nonnegative; a zero total requires zero completed,
and a positive total requires completed not to exceed total. Reporting blocks
until Airlock durably accepts the snapshot and returns any rejection to the
handler. It is available only on the `JobContext` and `context.Context` passed
to the active delivery attempt. A delivery without progress authorization still
runs, but `ReportProgress` returns an error.

```go
type ReportInput struct {
    AccountID string `json:"accountId"`
}
type ReportOutput struct {
    Path string `json:"path"`
}

reports := agentsdk.RegisterJob(agent, &agentsdk.Job[ReportInput, ReportOutput]{
    Name:           "generate_report",
    Version:        1,
    Description:    "Generate a report and store it.",
    Timeout:        10 * time.Minute,
    MaxAttempts:    3,
    MaxConcurrency: 2,
    Handler: func(ctx context.Context, job agentsdk.JobContext, in ReportInput) (ReportOutput, error) {
        // Commit effects idempotently under job.ID.
        return ReportOutput{Path: "reports/" + job.ID + ".pdf"}, nil
    },
})
```

Enqueue IDs are canonical non-nil UUIDs chosen by the caller. Retrying the same
ID while Airlock retains the job is idempotent and returns the existing job.
Airlock retains terminal jobs for 30 days; reusing an ID after retention creates
new work, so handlers must retain domain-level effect deduplication for as long
as their side effects require it. `Get` returns typed output after success.
`JobResult.Progress` contains the latest progress snapshot when one has been
reported. `JobResult.AttemptLimit` is the platform attempt cap, while
`MaxAttempts` is the contract's normal retry count. `Cancel` requests
cancellation of queued or running work.

During a deployment transition, `Enqueue` and `EnqueueAt` can return a
`*JobEnqueueUnavailableError`. Test it with
`errors.Is(err, agentsdk.ErrJobEnqueueUnavailable)` and retry the enclosing
operation. A job handler that returns or wraps this error is reported as
retryable to Airlock and receives a replacement delivery even when its normal
attempt budget is exhausted; other handler errors remain terminal.

```go
id := uuid.NewString()
result, err := reports.Enqueue(ctx, id, ReportInput{AccountID: accountID})
result, err = reports.EnqueueAt(ctx, id, when, ReportInput{AccountID: accountID})
result, err = reports.Get(ctx, id)
err = reports.Cancel(ctx, id)
```

`EnqueueAt` sends a durable delayed enqueue. The SDK requires a nonzero time and
normalizes it to UTC microsecond precision. Never use an in-process timer; the
container can suspend.

Attach recurring cron declarations to the exact job version they execute. Cron
slugs are lowercase snake_case and unique across the agent. The input is encoded
and synced as static typed input. Changing behavior or schemas requires a new job
version and an updated cron declaration.

```go
reports.Cron(&agentsdk.JobCron[ReportInput]{
    Slug:        "daily_report",
    Schedule:    "0 9 * * *",
    Input:       ReportInput{AccountID: "daily"},
    Description: "Generate the daily report.",
})
```
