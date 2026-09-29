# File storage

The app has host-backed logical file storage. Paths are slashless app-local names such as
`uploads/x.csv`, `reports/q1.pdf`, and `tmp/output.png`; they are never container
filesystem paths or object-store keys. Airlock maps paths to stable catalog
entries and immutable deduplicated content. Replacing a path updates that entry;
copies and materialized inputs are independent entries even when they initially
share content.

## Declaring directories

`RegisterDirectory` returns a `*DirectoryHandle` and requires independent read,
write, and list policies plus a description:

```go
uploads := agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
    Read:        agentsdk.AccessUser,
    Write:       agentsdk.AccessUser,
    List:        agentsdk.AccessUser,
    Description: "Uploaded source files",
})
```

`AccessInternal` makes a capability available only to explicitly selected
application-owned agents. It is not part of the human access hierarchy;
`AccessAdmin` does not satisfy it. Native Go code remains trusted with its app's
storage and can use every app-owned path directly.

`RetentionHours` opts a directory into age-based cleanup. `Scope` can partition
paths by run, conversation, or user. Use scoping only when caller isolation is a
requirement; it changes the physical path returned from writes.

The framework declares `tmp` as user-readable/writable/listable scratch with a
72-hour retention period. Calling `RegisterDirectory("tmp", ...)` returns a valid
handle and may customize its description without changing those framework
settings. Local `os.CreateTemp` and `os.MkdirTemp` remain appropriate for
process-local CLI scratch, but those paths must never be returned as `FilePath`.

## Materialized tool inputs

`FilePath` marks tool fields that name files. A tool accepting materialized file
inputs must declare one destination directory:

```go
agent.RegisterTool(convertTool, agentsdk.AccessUser,
    agentsdk.WithFileInputs(uploads))
```

Airlock materializes every `FilePath` input as an independent entry in that directory before
invoking the tool and rewrites the input to the resulting normal app-owned path.
The tool body uses `ResolveFilePath`, `OpenFile`, and the other app-owned storage
methods normally. There are no per-field destinations or implicit scratch
destinations. A nil, foreign, or unregistered directory handle is
rejected when registrations are validated.

`DirPath` marks directory-valued fields. Directory trees are not implicitly
materialized.

## Trusted Go API

Code that constructs its own paths can use the trusted app-owned methods:

```go
src, err := agent.OpenFile(ctx, "uploads/doc.pdf")
data, err := agent.ReadFile(ctx, "uploads/notes.txt")
info, err := agent.WriteFile(ctx, "reports/q1.csv", reader, "text/csv")
info, err := agent.StatFile(ctx, "uploads/doc.pdf")
ref, err := agent.StatFileRef(ctx, "uploads/doc.pdf")
ref, err = agent.SetFileIndex(ctx, ref, "searchable plain-text description")
files, err := agent.ListDir(ctx, "uploads", agentsdk.ListOpts{Recursive: false})
err := agent.DeleteFile(ctx, "reports/old.csv")
err := agent.CopyFile(ctx, "uploads/in.csv", "reports/copy.csv")
share, err := agent.ShareFileURL(ctx, "reports/q1.csv", time.Hour)
```

These methods do not call `ResolveFilePath`. Native app code is trusted with its
own storage. `FileRef.ID` remains stable across content replacement;
`FileRef.ContentID` is an optimistic fence. `SetFileIndex` fails if content
changed after `StatFileRef`, and an empty index restores automatic extraction.

## Untrusted paths

Resolve paths supplied by a model or another untrusted caller before using them:

```go
type Input struct {
    Source agentsdk.FilePath `json:"source"`
}

resolved, err := agent.ResolveFilePath(ctx, string(in.Source), agentsdk.FileOperationRead)
if err != nil {
    return Output{}, err
}
reader, err := agent.OpenFile(ctx, string(resolved))
```

`ResolveFilePath` returns `ErrNotFound` for both an uncovered path and denied
access so path guessing does not reveal registered storage. Delete uses the
directory's write policy.

## CLI scratch

CLI programs may need local files. Download the app-owned object to an
`os.CreateTemp` file, invoke the command with `exec.CommandContext`, upload the
result with `WriteFile`, and remove all local scratch on every exit path. Return
the uploaded `FileInfo.Path`, not the local filename.

## HTTP responses

Use `github.com/airlockrun/agentsdk/filehttp` when an app route streams stored
bytes. `filehttp.SetHeaders` applies safe content disposition and sandboxing for
active document formats.
