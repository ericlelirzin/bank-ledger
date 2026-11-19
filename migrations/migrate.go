// Package migrations applies embedded, versioned SQL migrations atomically.
package migrations

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed 001_ledger.sql
var initial string

//go:embed 002_reliability.sql
var reliability string

//go:embed 003_banking.sql
var banking string

//go:embed 004_beneficiaries.sql
var beneficiaries string

func Apply(ctx context.Context, db *pgxpool.Pool) error {
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(74209183)`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY)`); e != nil {
		return e
	}
	for i, sql := range []string{initial, reliability, banking, beneficiaries} {
		version := i + 1
		var exists bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); e != nil {
			return e
		}
		if exists {
			continue
		}
		if _, e = tx.Exec(ctx, sql); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO schema_migrations VALUES($1)`, version); e != nil {
			return e
		}
	}

	return tx.Commit(ctx)
}
