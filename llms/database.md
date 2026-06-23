# Database access

> Companion to `/libs/agentsdk/llms.md` — read that first. Come here when your task involves Postgres tables, sqlc, or goose migrations.

You have a full Postgres database available (well, a single schema, but you can
create as many tables in it as you like). Usually the database has pgvector
enabled, so you can create vector columns and use them together with
`agent.EmbeddingModel(ctx, slug)`.

If the agent needs its own database tables:

1. Migration files in `db/migrations/` (e.g. `00001_init.sql`)
2. Query files in `db/queries/` (e.g. `queries.sql`)
3. `sqlc generate` — produces Go code in `internal/db/`
4. Import `internal/db` in your code

Migrations run automatically at container startup via **goose**. Each `.sql`
file has Up and Down sections:

```sql
-- +goose Up
CREATE TABLE rooms (
    id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL
);

-- +goose Down
DROP TABLE rooms;
```

**Numbering:** zero-padded prefixes (`00001_init.sql`). Goose runs them in
numeric order.

**Go migrations** for operational work (rename S3 keys, backfill via HTTP, ...):
create a `.go` file in `db/migrations/`. Get the agent via
`agentsdk.AgentFromMigrationContext(ctx)`.

**Tx vs NoTx:**
- `goose.AddMigrationContext(up, down)` — wraps in a Postgres transaction.
  Default for short, DB-focused work.
- `goose.AddMigrationNoTxContext(up, down)` — no wrapping tx. Use when you
  (1) call slow external services (S3, HTTP) — don't hold a Postgres tx idle
  across them; or (2) need ops Postgres won't run in a tx
  (`CREATE INDEX CONCURRENTLY`, `VACUUM`, ...).

```go
// db/migrations/00002_rename_media.go
package migrations

import (
    "context"
    "database/sql"
    "path"

    "github.com/airlockrun/agentsdk"
    "github.com/pressly/goose/v3"
)

func init() {
    // NoTx: calls S3 in a loop; don't hold a Postgres tx open across slow external calls.
    goose.AddMigrationNoTxContext(Up00002, Down00002)
}

func Up00002(ctx context.Context, db *sql.DB) error {
    // Build-time validation runs migrations against a test DB without S3,
    // Airlock API, or connection credentials. Guard side effects so SQL still runs.
    if agentsdk.IsValidatingMigrations() {
        return nil
    }
    agent := agentsdk.AgentFromMigrationContext(ctx)
    files, err := agent.ListDir(ctx, "old/", agentsdk.ListOpts{Recursive: true})
    if err != nil {
        return err
    }
    for _, f := range files {
        src := string(f.Path)
        dst := "media/" + path.Base(src)
        if err := agent.CopyFile(ctx, src, dst); err != nil {
            return err
        }
        if err := agent.DeleteFile(ctx, src); err != nil {
            return err
        }
    }
    return nil
}

func Down00002(ctx context.Context, db *sql.DB) error { return nil }
```

`main.go` already blank-imports `db/migrations`, so `init()` fires
automatically.

**Guard external side effects.** Build-time validation runs the full migration
chain (up → down → up) against a test DB clone with no S3, Airlock API, or
connection credentials. Go migrations that touch external services must check
`agentsdk.IsValidatingMigrations()` and return early — but still run any
DB/schema work later migrations depend on.

**Validate after creating migrations** (Airlock builder; three env vars
`TEST_DB_URL` for goose, `TEST_DB_PSQL` for psql, `TEST_DB_SCHEMA` — skip if
`$TEST_DB_URL` is unset):

```bash
goose -dir db/migrations postgres "$TEST_DB_URL" up
goose -dir db/migrations postgres "$TEST_DB_URL" reset
goose -dir db/migrations postgres "$TEST_DB_URL" up

psql "$TEST_DB_PSQL" -c "SET search_path TO $TEST_DB_SCHEMA; SELECT table_name FROM information_schema.tables WHERE table_schema = '$TEST_DB_SCHEMA'"
```

The agent gets its own Postgres schema. `agent.DB()` returns an `*AgentDB`
wrapping `*sql.DB` — pass it straight to the generated `New()`.

**Using sqlc in Go:**

```go
db := agent.DB()
queries := internaldb.New(db) // import "agent/internal/db" as internaldb
users, err := queries.ListActiveUsers(ctx)
```

**Always use sqlc.** Never write raw `db.QueryRow`/`db.Exec` strings in Go.
