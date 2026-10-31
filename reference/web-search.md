# Web search from agent code

Run the search in Go with `agent.WebSearch(ctx, slug, req)` and put the results
in the message. Prefer this over handing the model a search *tool*: it's a
single round-trip (no model→Airlock→model detour), the search always runs (a
tool the model may decline to call doesn't), and you control exactly what
enters the context. If you need the model to decide whether to search
mid-conversation, wrap `agent.WebSearch` in your own `RegisterTool`.

```go
import (
    "github.com/airlockrun/goai/message"
    "github.com/airlockrun/goai/stream"
    "github.com/airlockrun/sol/websearch"
)

// once, before Serve():
agent.RegisterModel(&agentsdk.ModelSlot{
    Slug: "research", Capability: agentsdk.CapSearch,
    Description: "Web search for the weekly digest",
})

func Digest(ctx context.Context, in DigestIn) (DigestOut, error) {
    a := agentsdk.AgentFromContext(ctx)

    found, err := a.WebSearch(ctx, "research", websearch.Request{Query: in.Topic, Count: 5})
    if err != nil {
        return DigestOut{}, err
    }

    result, err := a.GenerateText(ctx, stream.Input{
        Model: a.LLM(ctx, "assistant"),
        Messages: []message.Message{
            message.NewUserMessage("Using these search results:\n" + found.Text() +
                "\nWrite a short digest of: " + in.Topic),
        },
    })
    if err != nil {
        return DigestOut{}, err
    }
    return DigestOut{Summary: result.Text}, nil
}
```

`found.Text()` returns the provider's synthesized answer when available
(Grok/Gemini/Kimi) and otherwise a bulleted list of the results — so you don't
have to branch on `found.Synthesis` or format `found.Results` by hand.
