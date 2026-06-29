# Auth flows that need user input — an admin web page

> Companion to `/libs/agentsdk/llms.md` — read that first. Come here when your task involves an interactive login (one-time code, password, click) that must be driven from an admin page.

Many credentials can't be minted headlessly: the login emits a one-time code,
asks for a password, or needs a click. That input is **ephemeral and
interactive** — it can't be an env var. Drive it from an `AccessAdmin`
`RegisterRoute` page (templ + htmx): an admin performs the interactive step, the
agent finishes the login, and you `Seal` the resulting long-lived credential.
Later runs `Unseal` it and never prompt again.

**Worked example — a messaging CLI (`msgcli`) the agent shells out to.** It
needs API credentials (env) *and* an interactive phone-code login (admin page),
and it authenticates with a session string we seal:

```go
// Config the binary reads from its environment. api_hash is a credential.
var (
    apiID = agent.RegisterEnvVar(&agentsdk.EnvVar{
        Slug: "msg_api_id", Description: "API ID from the provider console",
        Pattern: `^[0-9]+$`,
    })
    apiHash = agent.RegisterEnvVar(&agentsdk.EnvVar{
        Slug: "msg_api_hash", Description: "API hash from the provider console",
        Secret: true, Pattern: `^[a-f0-9]{32}$`,
    })
)

const sessionPath = "state/msg-session.enc" // sealed blob, agent-wide

// runCLI shells out with env + the unsealed session (when one exists).
func runCLI(ctx context.Context, args ...string) ([]byte, error) {
    id, err := apiID.Get(ctx)
    if err != nil {
        return nil, fmt.Errorf("set msg_api_id in the Environment tab: %w", err)
    }
    hash, err := apiHash.Get(ctx)
    if err != nil {
        return nil, fmt.Errorf("set msg_api_hash in the Environment tab: %w", err)
    }
    env := append(os.Environ(), "MSG_API_ID="+id, "MSG_API_HASH="+hash)

    if blob, err := agent.ReadFile(ctx, sessionPath); err == nil {
        session, err := agent.Unseal(ctx, string(blob))
        if err != nil {
            return nil, fmt.Errorf("stored session unreadable — re-run login: %w", err)
        }
        env = append(env, "MSG_SESSION="+session)
    }
    cmd := exec.CommandContext(ctx, "msgcli", args...)
    cmd.Env = env
    return cmd.CombinedOutput()
}

// --- admin login page: the only place the plaintext session exists ---

// GET /admin/login — render the phone form.
agent.RegisterRoute(&agentsdk.Route{
    Method: "GET", Path: "/admin/login", Access: agentsdk.AccessAdmin,
    Description: "Interactive login page for the messaging account",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        templ.Handler(views.PhoneForm()).ServeHTTP(w, r)
    },
})

// POST /admin/login/start — CLI sends a one-time code to the operator's device.
agent.RegisterRoute(&agentsdk.Route{
    Method: "POST", Path: "/admin/login/start", Access: agentsdk.AccessAdmin,
    Description: "Begin login: trigger the one-time code",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        phone := r.FormValue("phone")
        if _, err := runCLI(ctx, "login", "start", "--phone", phone); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        views.CodeForm(phone).Render(ctx, w) // htmx swaps in the code form
    },
})

// POST /admin/login/verify — finish login, seal the session, persist it.
agent.RegisterRoute(&agentsdk.Route{
    Method: "POST", Path: "/admin/login/verify", Access: agentsdk.AccessAdmin,
    Description: "Finish login and store the sealed session",
    Handler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
        out, err := runCLI(ctx, "login", "verify",
            "--phone", r.FormValue("phone"), "--code", r.FormValue("code"))
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        session := strings.TrimSpace(string(out)) // the long-lived session string

        sealed, err := agent.Seal(ctx, session)
        if err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        if _, err := agent.WriteFile(ctx, sessionPath, strings.NewReader(sealed), "text/plain"); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        views.LoginDone().Render(ctx, w)
    },
})

// The LLM-facing tool just runs the binary; the session is wired in by runCLI.
agent.RegisterTool(tool.Typed[SendIn, SendOut]("send_message").
    Description("Send a message via the linked account.").
    Execute(func(ctx context.Context, in SendIn) (SendOut, error) {
        out, err := runCLI(ctx, "send", "--to", in.To, "--text", in.Text)
        if err != nil {
            return SendOut{}, fmt.Errorf("not linked yet — an admin must complete login at the agent's /admin/login page: %w", err)
        }
        return SendOut{Result: string(out)}, nil
    }).
    Build(), agentsdk.AccessUser)
```

For **per-user** login — the SaaS-style case where the agent's own end users
each link their account through public signup/login pages the agent serves —
Airlock supplies no caller identity to lean on. The agent runs its own auth on
`AccessPublic` routes, keeps a `users` table and sessions in its own Postgres
schema, and keys each sealed session by *its* `user_id`. `runCLI` resolves the
current user from the agent's session (cookie / token / however its pages
identify the request) and loads that row instead of `ReadFile`. The
seal/unseal calls are identical — only where you keep the ciphertext, and how
you identify whose it is, changes.

