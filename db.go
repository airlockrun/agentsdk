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
// Builders that need the underlying driver-level handle for advanced cases
// (Stats, Driver, custom drivers) can reach it via Underlying().
type AgentDB struct {
	db    *sql.DB
	agent *Agent
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

// BeginTx starts a transaction. The returned *AgentTx implements the same
// DBTX interface, so `mygen.New(tx)` and `q.WithTx(tx)` both work.
func (a *AgentDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*AgentTx, error) {
	tx, err := a.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &AgentTx{tx: tx, agent: a.agent}, nil
}

// Underlying returns the wrapped *sql.DB. Use this only when you need
// driver-level access that DBTX doesn't expose; otherwise stick to the
// AgentDB methods so future framework instrumentation applies to your code.
func (a *AgentDB) Underlying() *sql.DB { return a.db }

// AgentTx wraps a *sql.Tx with the same shape as AgentDB so sqlc's
// q.WithTx(tx) accepts it transparently.
type AgentTx struct {
	tx    *sql.Tx
	agent *Agent
}

// ExecContext satisfies sqlc's DBTX.
func (a *AgentTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return a.tx.ExecContext(ctx, query, args...)
}

// PrepareContext satisfies sqlc's DBTX.
func (a *AgentTx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return a.tx.PrepareContext(ctx, query)
}

// QueryContext satisfies sqlc's DBTX.
func (a *AgentTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return a.tx.QueryContext(ctx, query, args...)
}

// QueryRowContext satisfies sqlc's DBTX.
func (a *AgentTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return a.tx.QueryRowContext(ctx, query, args...)
}

// Commit commits the transaction.
func (a *AgentTx) Commit() error { return a.tx.Commit() }

// Rollback aborts the transaction. Idempotent — safe to defer.
func (a *AgentTx) Rollback() error { return a.tx.Rollback() }

// Underlying returns the wrapped *sql.Tx.
func (a *AgentTx) Underlying() *sql.Tx { return a.tx }
