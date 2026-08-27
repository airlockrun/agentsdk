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

**Go migrations** are for bounded PostgreSQL changes that require Go logic.
Create a `.go` file in `db/migrations/` and register it with goose. Migration
callbacks only operate on PostgreSQL; storage, HTTP, credentials, and other
external systems belong in durable jobs.

**Tx vs NoTx:**
- `goose.AddMigrationContext(up, down)` — wraps in a Postgres transaction.
  Default for short, DB-focused work.
- `goose.AddMigrationNoTxContext(up, down)` — no wrapping transaction. Use only
  for PostgreSQL operations that cannot run in a transaction, such as
  `CREATE INDEX CONCURRENTLY` or `VACUUM`.

```go
// db/migrations/00002_backfill_room_slugs.go
package migrations

import (
    "context"
    "database/sql"

    db "agent/internal/db"
    "github.com/pressly/goose/v3"
)

func init() {
    goose.AddMigrationContext(Up00002, Down00002)
}

func Up00002(ctx context.Context, tx *sql.Tx) error {
    return db.New(tx).BackfillRoomSlugs(ctx)
}

func Down00002(ctx context.Context, tx *sql.Tx) error {
    return db.New(tx).ClearBackfilledRoomSlugs(ctx)
}
```

Generated `scaffold_gen.go` blank-imports `db/migrations`, so `init()` fires in
the agent binary and root-package tests. A subpackage test binary that exercises
Go migrations must also blank-import `agent/db/migrations`; prefer an external
test package if a migration imports the package under test. SQL migrations need
no import.

Keep Go migrations bounded. They run during startup, hold the per-agent migration
lock, and must finish before the agent can serve. Large database backfills should
also use jobs over an expand/migrate/contract schema when they cannot fit safely
inside that startup window.

**External data changes use durable jobs.** S3 and API operations cannot commit
atomically with goose, cannot be meaningfully exercised by migration validation,
and can take an unbounded amount of time. Register a versioned job, enqueue it
from an explicit admin action, report progress, and keep the application
compatible with both layouts until it completes. Persist the generated job ID
in an app-owned maintenance row before calling `Enqueue` so a lost response
cannot orphan accepted work; repeated actions can call `Get` instead of creating
duplicates. See
`/libs/agentsdk/reference/jobs.md` for the complete pattern.

**Validate SQL-only migrations after creating them** (Airlock builder; three env vars
`TEST_DB_URL` for goose, `TEST_DB_PSQL` for psql, `TEST_DB_SCHEMA` — skip if
`$TEST_DB_URL` is unset):

```bash
goose -dir db/migrations postgres "$TEST_DB_URL" up
goose -dir db/migrations postgres "$TEST_DB_URL" reset
goose -dir db/migrations postgres "$TEST_DB_URL" up

psql "$TEST_DB_PSQL" -c "SET search_path TO $TEST_DB_SCHEMA; SELECT table_name FROM information_schema.tables WHERE table_schema = '$TEST_DB_SCHEMA'"
```

The standalone goose CLI cannot register numbered Go migrations. If any `.go`
migration exists, use `go tool air build` for source verification. Airlock then
runs the compiled image through up, down, and up before deployment, which
validates SQL and registered database-only Go migrations together.

The agent gets its own Postgres schema. `agentsdk.New` does not read
`AIRLOCK_DB_URL`, open a pool, or run migrations. `agent.DB()` returns a stable,
late-bound `*AgentDB`; pass it straight to generated sqlc constructors while
wiring dependencies, but do not execute database operations from the factory.
Operations become available when `Start` opens and checks the owned pool and
runs migrations. `Serve` calls `Start` and closes the pool during shutdown.

`agenttest.New` invokes the factory before it provisions runtime dependencies,
then resolves source `db/migrations` from the nearest enclosing Go module,
starts the agent, validates migrations with an up, down-to-zero, up cycle, and
returns after sync and `OnStart` hooks. DB-backed tests therefore work from
subpackages without changing process cwd. The final up leaves the schema ready
for the test. The SDK build runs packages serially because package test binaries
sharing one `TEST_DB_URL` must not reset the same schema concurrently. `go tool
air build` provisions one throwaway pgvector container and supplies its URL to
that serial test run; direct `go test` calls without `TEST_DB_URL` let each
`agenttest.New` provision its own container.

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
