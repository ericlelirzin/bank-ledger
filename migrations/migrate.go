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

func Apply(migrationContext context.Context, databasePool *pgxpool.Pool) error {
	transaction, err := databasePool.Begin(migrationContext)
	if err != nil {
		return err
	}
	defer func() {
		cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCleanup()
		_ = transaction.Rollback(cleanupContext)
	}()
	if _, err = transaction.Exec(migrationContext, `SELECT pg_advisory_xact_lock(74209183)`); err != nil {
		return err
	}
	if _, err = transaction.Exec(migrationContext, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY)`); err != nil {
		return err
	}
	for migrationIndex, migrationSQL := range []string{initial, reliability} {
		version := migrationIndex + 1
		var alreadyApplied bool
		if err = transaction.QueryRow(migrationContext, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&alreadyApplied); err != nil {
			return err
		}
		if alreadyApplied {
			continue
		}
		if _, err = transaction.Exec(migrationContext, migrationSQL); err != nil {
			return err
		}
		if _, err = transaction.Exec(migrationContext, `INSERT INTO schema_migrations VALUES($1)`, version); err != nil {
			return err
		}
	}

	return transaction.Commit(migrationContext)
}
