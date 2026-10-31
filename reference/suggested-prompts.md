# Suggested chat prompts

App-building agents should add **1–3 useful, app-specific prompts** during
registration. Choose meaningful workflows the app actually supports, not generic
assistant tasks. Use the app's chosen language; titles and prompts display verbatim.

```go
agent.RegisterSuggestedPrompt(agentsdk.SuggestedPrompt{
    Title: "Find a product",
    Prompt: "Help me find a product in the catalogue.",
})
```

`RegisterSuggestedPrompt` accepts a value with a concise `Title` and a full,
editable `Prompt`. Declarations retain registration order in the manifest and
runtime sync. Airlock shows only authorized active production apps, with app
attribution, and shows the selected app's prompts in an empty app chat, within
the conversation's capability policy. Clicking
opens/selects that app's chat workspace and fills the composer, **without sending**.
Existing composer text is preserved, with the suggestion appended as a paragraph.
Apps without declarations have no placeholder suggestions. These static strings
execute no code and grant no permissions; sending uses normal live authorization.

The hard limits are 6 prompts per app, 120 UTF-8 bytes per title, and 4000 UTF-8
bytes per prompt. Both fields must be nonblank valid UTF-8. Control characters
are rejected except newline/tab in prompt bodies. Invalid registration panics;
the host rejects invalid manifests atomically. Register before `Manifest`,
`Handler`, or `Serve` freezes declarations. Optional suggestions use the existing
`airlock.app-runtime.v4` contract and require no application source migration.
