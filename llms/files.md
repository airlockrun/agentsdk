# File storage — RegisterDirectory & file access

> Companion to `/libs/agentsdk/llms.md` — read that first. Come here when your task involves reading/writing the agent's object storage or handling LLM-supplied paths.

The agent has its own **S3-like object storage**. There is no container
filesystem you expose to tools or to the LLM — every path you read or write is
an S3 key. Paths are slashless: `uploads/x.csv`, `reports/q1.pdf`,
`tmp/foo.png`. Leading slashes are rejected by the SDK (the LLM and your Go
code share one canonical form).

**Hard rule for tool authors: tool inputs and outputs are *storage* paths,
never container paths.** A `Source agentsdk.FilePath` field on a tool's `In`
struct is a storage path the LLM (or a sibling agent over A2A) passed in;
convert with `string(in.Source)` and pass it through `CheckFileAccess` /
`agent.OpenFile`. A `Result agentsdk.FilePath` you return is a storage path
the LLM, chat, or calling sibling will follow back through the same storage
namespace. Returning `os.CreateTemp` paths or `localOut.Name()` to the LLM
gives it a path it cannot read — the framework will 404. When you need a real
on-disk file (CLI tools like `ffmpeg`, `pdftotext`), use `os.CreateTemp`
purely as scratch inside the tool body, then upload the result with
`agent.WriteFile` and return the resulting path as `agentsdk.FilePath`.

```go
agent.RegisterDirectory("uploads", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessUser, List: agentsdk.AccessUser,
    Description: "Files the user uploaded in chat.",
})
agent.RegisterDirectory("reports", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessUser, Write: agentsdk.AccessAdmin, List: agentsdk.AccessUser,
    Description: "Generated reports.",
})
agent.RegisterDirectory("cache", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Description: "Memoized responses.",
    LLMHint:     "internal cache; do not read, write, or list",
})
agent.RegisterDirectory("generated-images", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Description:    "Throwaway AI-generated images served via fileShareURL.",
    LLMHint:        "served externally via fileShareURL only; do not read or list directly",
    RetentionHours: 24, // sweeper drops files older than 24h
})
```

`Read` / `Write` / `List` are independent caps. `delete` folds into `Write`
(write on the parent governs unlink). Each can be `AccessAdmin`, `AccessUser`,
or `AccessPublic`. To hide a directory from the LLM while keeping it reachable
from your Go code, set the caps to `AccessAdmin` and add an `LLMHint` like
`"internal cache; do not read, write, or list"` — the hint is purely
model-facing guidance surfaced in the system prompt, while the trusted Go file
API (`agent.OpenFile`/`ReadFile`/...) bypasses `CheckFileAccess` entirely so
your code can use the directory freely.

**`RetentionHours`** opts the directory into Airlock's storage sweeper: any
object older than the configured hours is deleted on the ~6h sweep tick.
Default `0` means files live forever — that's right for `uploads`, `reports`,
anything the user expects to find tomorrow. Set a TTL on directories that hold
ephemeral artifacts (generated media, transient scratch, presigned-URL
targets) so the bucket doesn't grow without bound. Match the TTL to whatever
URL expiry you hand out: a `fileShareURL(path, {expiresInMinutes: 60})` link is
useless after an hour, so the bytes can go away on roughly the same horizon.

**`Scope`** — **use rarely.** Default (`ScopeNone`) is correct for almost every
directory; reach for scoping only when the user explicitly asks for per-user
(or per-conversation) file isolation, or when a tool that produces files must
be exposed at the public tier. Don't scope `uploads`, `reports`, `cache`, or
general working directories — that breaks the natural "the agent and its
members share these files" model and surprises the user when files "disappear"
across conversations.

When you do need it: `Scope` opts the directory into per-context path
isolation. WriteFile inserts a scope segment (`user-<id>` / `conv-<id>` /
`run-<id>`) under the directory prefix; CheckFileAccess accepts the matching
segment on reads. The author's code is unchanged —
`agent.WriteFile(ctx, "gen/cat.jpg", ...)` returns the scoped path, the LLM
passes it around, and reads just work for the caller who wrote it. Use this
instead of opening a directory to `AccessPublic` when a public-tier tool
produces files: `Scope: ScopeUser` (with `Read/Write/List=AccessAdmin`) keeps
the base ACL locked while letting each caller see their own slot. `ScopeUser`
falls back to conv → run when no logged-in user is available, so anon callers
still get isolation.

```go
agent.RegisterDirectory("gen", agentsdk.DirectoryOpts{
    Read: agentsdk.AccessAdmin, Write: agentsdk.AccessAdmin, List: agentsdk.AccessAdmin,
    Scope:          agentsdk.ScopeUser,
    Description:    "Per-caller generated artifacts.",
    RetentionHours: 24,
})
// tool body:
out, _ := agent.WriteFile(ctx, "gen/result.png", buf, "image/png")
// out.Path is "gen/user-<uuid>/result.png" — return that to the LLM.
```

The framework auto-registers `tmp` at `Read=Write=List=AccessUser` with
`RetentionHours: 72` (unscoped — accessible to any user-tier caller). You may
`RegisterDirectory("tmp", DirectoryOpts{Description: "..."})` to customize the
description; the access caps, retention, and unscoped behaviour stay at the
framework's values.

### Trusted Go file API — for code that constructs paths

```go
src,  err := agent.OpenFile(ctx, "uploads/doc.pdf")          // io.ReadCloser
data, err := agent.ReadFile(ctx, "uploads/notes.txt")        // []byte
info, err := agent.WriteFile(ctx, "reports/q1.csv", reader, "text/csv")
info, err := agent.StatFile(ctx, "uploads/doc.pdf")          // FileInfo
files, err := agent.ListDir(ctx, "uploads/", agentsdk.ListOpts{Recursive: false})
err := agent.DeleteFile(ctx, "reports/old.csv")              // idempotent
err := agent.CopyFile(ctx, "uploads/in.csv", "reports/copy.csv")
share, err := agent.ShareFileURL(ctx, "reports/q1.csv", time.Hour) // {URL, ExpiresAtMs}
```

These do **not** call `CheckFileAccess` — agent Go is trusted with paths it
constructs itself. Use freely from cron/webhook handlers, internal caches,
anywhere.

`ShareFileURL` returns a presigned, unauthenticated, time-limited URL (default
1h, capped at 24h server-side). Use it from cron/webhook handlers when you need
to email/post a one-off external link to a stored file. For chat delivery,
prefer `printToUser({type:"file", source:path})` from `run_js`; for member-only
access on a stable URL, the agent's `__air/storage/{path}` route already
handles it.

### Untrusted paths — when a tool accepts a path from the LLM

If a tool input declares a path field, the path is **untrusted**. The LLM can
pass any string, including paths under internal directories or with `..`
segments. **Call `agent.CheckFileAccess` before using the path anywhere** —
even before passing it to an external API.

```go
type CropIn struct {
    Image   agentsdk.FilePath `json:"image"`
    X, Y, W, H int
}
type CropOut struct {
    Result agentsdk.FilePath `json:"result"`
}

agent.RegisterTool(&agentsdk.Tool[CropIn, CropOut]{
    Name:        "crop_image",
    Description: "Crop a stored image and save the result.",
    Access:      agentsdk.AccessUser,
    Execute: func(ctx context.Context, in CropIn) (CropOut, error) {
        src := string(in.Image) // FilePath → string for the file API
        if err := agent.CheckFileAccess(ctx, src, agentsdk.OpRead); err != nil {
            return CropOut{}, err
        }
        r, err := agent.OpenFile(ctx, src)
        if err != nil {
            return CropOut{}, err
        }
        defer r.Close()
        cropped := crop(r, in.X, in.Y, in.W, in.H)
        info, err := agent.WriteFile(ctx, "tmp/crop-"+randHex(4)+".png",
            bytes.NewReader(cropped), "image/png")
        if err != nil {
            return CropOut{}, err
        }
        return CropOut{Result: info.Path}, nil
    },
})
```

`CheckFileAccess` returns `agentsdk.ErrNotFound` for both "denied" and "no
covering directory" — the indistinguishable response prevents path-guessing
from leaking what exists.

### Shelling out to a CLI tool

CLI tools (ffmpeg, pdftotext, imagemagick, libreoffice, …) need real on-disk
files. The shape is always: permission-check the LLM-supplied storage path,
download to a temp file, shell out, upload the result back to storage, clean
up. Temp files are scratch internal to the tool — they never appear in inputs
or outputs.

```go
type TranscodeIn struct {
    Source agentsdk.FilePath `json:"source"`
}
type TranscodeOut struct {
    Result agentsdk.FilePath `json:"result"`
}

Execute: func(ctx context.Context, in TranscodeIn) (TranscodeOut, error) {
    srcPath := string(in.Source) // FilePath → string for the file API
    if err := agent.CheckFileAccess(ctx, srcPath, agentsdk.OpRead); err != nil {
        return TranscodeOut{}, err
    }

    // Download the storage object into a scratch temp file.
    src, err := agent.OpenFile(ctx, srcPath)
    if err != nil {
        return TranscodeOut{}, err
    }
    defer src.Close()
    inFile, err := os.CreateTemp("", "in-*"+filepath.Ext(srcPath))
    if err != nil {
        return TranscodeOut{}, err
    }
    defer os.Remove(inFile.Name())
    if _, err := io.Copy(inFile, src); err != nil {
        inFile.Close()
        return TranscodeOut{}, err
    }
    inFile.Close()

    // Run the CLI against the scratch files.
    outFile, _ := os.CreateTemp("", "out-*.mp3")
    outFile.Close()
    defer os.Remove(outFile.Name())
    cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inFile.Name(),
        "-b:a", "128k", outFile.Name())
    if out, err := cmd.CombinedOutput(); err != nil {
        return TranscodeOut{}, fmt.Errorf("ffmpeg: %w: %s", err, out)
    }

    // Upload the result back into storage and return the path as FilePath
    // so airlock auto-copies it to the caller across A2A boundaries.
    result, err := os.Open(outFile.Name())
    if err != nil {
        return TranscodeOut{}, err
    }
    defer result.Close()
    info, err := agent.WriteFile(ctx, "transcoded/"+filepath.Base(outFile.Name()),
        result, "audio/mpeg")
    if err != nil {
        return TranscodeOut{}, err
    }
    return TranscodeOut{Result: info.Path}, nil
},
```

Skipping any step either leaks across the security boundary or leaves the
result invisible to the rest of the agent:

- The two `defer os.Remove` calls fire on every exit path; without them the
  container's scratch disk fills up over time.
- Never return `outFile.Name()` (or any `os.CreateTemp` path) to the LLM. That
  path doesn't exist in the agent's storage; `agent.StatFile` would 404. Always
  upload with `agent.WriteFile` and return the resulting path as
  `agentsdk.FilePath`.
- For tools whose output filename you don't control, point the CLI at a temp
  *directory* (`os.MkdirTemp`), `defer os.RemoveAll` it, then walk and upload.

> **Use ctx-aware primitives in tool Execute.** The `ctx` in `Execute(ctx, in)`
> cancels when the user cancels or the run timeout fires.
> `exec.CommandContext`, `http.NewRequestWithContext`,
> `agent.DB().QueryContext(ctx, ...)` (and sqlc query methods, which take
> `ctx`), and
> `select { case <-ctx.Done(): ...; case <-time.After(d): }` honor it;
> `time.Sleep`, `io.ReadAll` without ctx, and blocking syscalls don't. A
> non-cooperative tool keeps running until it returns naturally — the run row
> finalizes correctly, but cancellation feels broken to the user.

### Public URLs for storage

Every registered directory is reachable at
`https://{slug}.{agentDomain}/__air/storage/{path}`; the proxy enforces the
directory's `Read` cap at fetch time:

- `AccessPublic` — unauthenticated, anyone with the URL.
- `AccessUser` — requires agent membership (proxy redirects through
  relay-login).
- `AccessAdmin` — same, admin role.

