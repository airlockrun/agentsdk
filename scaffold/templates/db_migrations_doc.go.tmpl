// Package migrations contains database schema and bounded data migrations
// applied at agent startup via goose.
//
// SQL migrations: create files like 00002_my_change.sql with
// -- +goose Up / -- +goose Down sections:
//
//	-- +goose Up
//	CREATE TABLE rooms (id uuid PRIMARY KEY, name text NOT NULL);
//
//	-- +goose Down
//	DROP TABLE rooms;
//
// Go migrations: for bounded PostgreSQL work that requires Go logic. External
// storage, HTTP, and credential work belongs in a durable registered job so it
// does not block startup and has observable attempts and progress. Create files
// like 00003_backfill_rooms.go:
//
//	package migrations
//
//	import (
//		"context"
//		"database/sql"
//
//		db "agent/internal/db"
//		"github.com/pressly/goose/v3"
//	)
//
//	func init() {
//		goose.AddMigrationContext(Up00003, Down00003)
//	}
//
//	func Up00003(ctx context.Context, tx *sql.Tx) error {
//		return db.New(tx).BackfillRoomSlugs(ctx)
//	}
//
//	func Down00003(ctx context.Context, tx *sql.Tx) error {
//		return db.New(tx).ClearBackfilledRoomSlugs(ctx)
//	}
//
// AddMigrationNoTxContext is reserved for PostgreSQL operations that cannot run
// in a transaction, such as CREATE INDEX CONCURRENTLY. Keep all migrations
// bounded; large database backfills also belong in jobs over a compatible schema.
package migrations
