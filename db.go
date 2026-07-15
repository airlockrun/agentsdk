package agentsdk

import (
	"context"
	"database/sql"
)

// AgentDB wraps the agent's *sql.DB and is the type returned by Agent.DB().
// It implements the same DBTX interface that sqlc-generated New() functions
// take, so builder code that does `mygen.New(agent.DB())` keeps compiling.
//
// Today AgentDB is a thin pass-through. The reason it exists is so the
// framework can later intercept queries at this layer (record an action on
// the run carried by ctx, surface query timings in the Runs UI, redact
// sensitive arguments) without breaking builders or sqlc-generated code.
type AgentDB struct {
	db    *sql.DB
	agent *Agent
}

// DBTX is the database surface accepted by sqlc-generated constructors. Both
// the agent pool and transaction callback values implement it.
type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	PrepareContext(context.Context, string) (*sql.Stmt, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ExecContext satisfies sqlc's DBTX. Forwards to the underlying *sql.DB.
func (a *AgentDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.db.ExecContext(ctx, query, args...)
}

// PrepareContext satisfies sqlc's DBTX. Forwards to the underlying *sql.DB.
func (a *AgentDB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return a.db.PrepareContext(ctx, query)
}

// QueryContext satisfies sqlc's DBTX. Forwards to the underlying *sql.DB.
func (a *AgentDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.db.QueryContext(ctx, query, args...)
}

// QueryRowContext satisfies sqlc's DBTX. Forwards to the underlying *sql.DB.
func (a *AgentDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return a.db.QueryRowContext(ctx, query, args...)
}

// PingContext checks database connectivity.
func (a *AgentDB) PingContext(ctx context.Context) error {
	return a.db.PingContext(ctx)
}

// Transaction runs fn in a transaction. The callback receives the DBTX shape
// accepted by sqlc-generated New functions. A nil callback panics; returning an
// error rolls back, and returning nil commits.
func (a *AgentDB) Transaction(ctx context.Context, opts *sql.TxOptions, fn func(DBTX) error) error {
	if fn == nil {
		panic("agentsdk: AgentDB.Transaction requires a callback")
	}
	tx, err := a.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// queryReadOnly runs query inside a read-only transaction that is always
// rolled back, then materializes the rows. This is the single enforcement
// point for the admin queryDB binding/tool: Postgres refuses every write
// (INSERT/UPDATE/DELETE/DDL/nextval/...) inside a READ ONLY transaction, so a
// mutating statement fails loudly rather than persisting, and the unconditional
// rollback guarantees nothing the query touched survives even if a future
// driver stopped honoring the flag. No commit path exists here by design.
func queryReadOnly(ctx context.Context, db *AgentDB, query string, params ...any) ([]map[string]any, error) {
	tx, err := db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rowsToMaps(rows)
}
