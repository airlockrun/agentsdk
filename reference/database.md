# Database access

> Companion to `/libs/agentsdk/REFERENCE.md` — read that first. Come here when your task involves Postgres tables, sqlc, or goose migrations.

You have a full Postgres database available (well, a single schema, but you can
create as many tables in it as you like). Usually the database has pgvector
enabled, so you can create vector columns and use them together with
`agent.EmbeddingModel(ctx, slug)`.

If the agent needs its own database tables:

1. Migration files in `db/migrations/` (e.g. `00001_init.sql`)
2. Query files in `db/queries/` (e.g. `queries.sql`)
3. `go tool air build` — conditionally runs pinned sqlc and produces Go code
   in `internal/db/`
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
    return agentsdk.MigrationExternalStep(ctx, func(ctx context.Context, agent *agentsdk.Agent) error {
        files, err := agent.ListDir(ctx, "old/", agentsdk.ListOpts{Recursive: true})
        if err != nil {
            return err
        }
        for _, file := range files {
            src := string(file.Path)
            dst := "media/" + path.Base(src)
            if err := agent.MoveFile(ctx, src, dst); err != nil {
                return err
            }
        }
        return nil
    })
}

func Down00002(ctx context.Context, db *sql.DB) error { return nil }
```

`main.go` already blank-imports `db/migrations`, so `init()` fires
automatically.

**Wrap external side effects.** `MigrationExternalStep` skips its callback while
build-time validation runs up, down, and up against a test DB without S3,
Airlock API, or connection credentials. Keep SQL work outside the callback when
later migrations depend on it.

A goose `NoTx` migration and an external service cannot commit atomically.
External steps have at-least-once execution semantics: if a process stops after
an external call succeeds but before goose records the version, the callback
can run again on startup. The SDK does not automatically retry or checkpoint
arbitrary external work. Make callbacks repeat-safe.
`Agent.MoveFile` implements repeat-safe storage moves: it copies then deletes
when the source exists, treats source-missing/destination-present as complete,
and returns `ErrNotFound` when both are absent. `ListDir` returns `[]FileInfo`;
use `string(file.Path)` as the canonical object key.

**Validate after creating migrations** (Airlock builder; three env vars
`TEST_DB_URL` for goose, `TEST_DB_PSQL` for psql, `TEST_DB_SCHEMA` — skip if
`$TEST_DB_URL` is unset):

```bash
goose -dir db/migrations postgres "$TEST_DB_URL" up
goose -dir db/migrations postgres "$TEST_DB_URL" reset
goose -dir db/migrations postgres "$TEST_DB_URL" up

psql "$TEST_DB_PSQL" -c "SET search_path TO $TEST_DB_SCHEMA; SELECT table_name FROM information_schema.tables WHERE table_schema = '$TEST_DB_SCHEMA'"
```

The agent gets its own Postgres schema. `AIRLOCK_DB_URL` is required, and
`agentsdk.New` opens, checks, and migrates one owned pool before returning.
`agent.DB()` always returns that `*AgentDB`; pass it straight to generated
sqlc constructors. `Serve` closes the pool during shutdown.

Each startup migration pass has a bounded context and holds a PostgreSQL
advisory lock keyed by agent ID from its first goose operation through its last.
This serializes replicas of one agent, including validation and down modes,
without blocking unrelated agents. Connection retries happen only during the
pre-migration ping; the SDK never retries a whole migration pass.

**Using sqlc in Go:**

```go
db := agent.DB()
queries := internaldb.New(db) // import "agent/internal/db" as internaldb
users, err := queries.ListActiveUsers(ctx)
```

Use `AgentDB.Transaction` for atomic work. Construct sqlc queries from the
callback value; the SDK commits only when the callback returns nil:

```go
err := agent.DB().Transaction(ctx, nil, func(tx agentsdk.DBTX) error {
    qtx := internaldb.New(tx)
    return qtx.CompleteTask(ctx, taskID)
})
```

**Always use sqlc.** Never write raw `db.QueryRow`/`db.Exec` strings in Go.
