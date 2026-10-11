# Stored JavaScript

`run_js_file` is a top-level model tool alongside `run_js`, not a JavaScript
callback. It executes a saved UTF-8 `.js` async function body:

```json
{"path":"extraction_artifacts/scripts/collect.js","args":{"pages":[9,10,11,12]},"description":"Inspect requested PDF pages"}
```

The body receives a structured lexical `args` object. Argument bytes travel over
an invocation-local intrinsic on the existing framed callback protocol; they are
never interpolated into JavaScript source. The same Deno realm, sandbox,
capability allowlist, cancellation, call/step/token/time budgets and bounded
logs/results as `run_js` apply. There is no nested execution callback, shell,
ambient host filesystem access, or TypeScript transpilation.

Bodies are capped at 256 KiB and object arguments at 64 KiB. `.ts`, noncanonical
paths, unreadable paths, invalid UTF-8, empty bodies and oversized inputs fail.
`expected_sha256` optionally fences the exact loaded bytes. Results carry
`source_path`, `source_sha256`, `args`, and available source content identity in
tool metadata. Console logs, JSON/undefined output, exceptions and attachment
references follow normal script execution behavior.

Task agents require `CapabilityFileRead`; selecting script execution grants no
extra callback. The loader invokes that exact authorized read operation against
registered app-local storage and normal internal/scoped directory policies.
Without a file-read capability the task tool is not advertised and cannot be
invoked through the model loop. Native private callbacks can read original
admission with `AgentHandle.Input(ctx)` and keep enforcing domain bounds.

Hosted conversations use the platform Files read operation with the live caller's
ACLs. For example, `/home/Scripts/summarize.js` addresses the caller-authorized
home catalog, not an OS path. App tasks have no human home identity. Local CLI
execution supplies app storage, not home sharing/indexing.

Use `request_confirmation` for effects requiring review. Approval is bound to
the loaded source hash; a replacement script cannot inherit that approval.
Session-local globals remain ephemeral across completion, suspension and waits.

Example body:

```js
const pages = [];
for (const page of args.pages) {
  const p = await tools.pdf_page({page});
  await air.attachToContext({path:p.image});
  pages.push({page:p.page, role:p.role, text:p.text});
}
return {pages};
```

Save it through existing `air.fileWrite({path:...,data:code})` inside `run_js`,
then invoke top-level `run_js_file`. In hosted global chat,
`conversation.files.write({path:"/home/Scripts/summarize.js",data:code})` uses
the platform Files API; its parent directory must exist and replacements require
the current contentId. Read authority does not imply write authority.

Host adapters can use `chatruntime.NewScriptFileTool` with a live `ScriptBackend`.
Registered task runtimes use `chatruntime.JavaScriptTools`, sharing one lazily
allocated realm between inline and saved bodies. `jsexec.ArgumentSession` extends
the executor without changing the existing Session interface or packaged
supervisor/worker protocol. Existing reviewed executor images remain compatible.
