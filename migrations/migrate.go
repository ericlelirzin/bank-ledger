// Package migrations applies embedded, versioned SQL migrations atomically.
package migrations

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Embed SQL in the binary so startup does not depend on the working directory.
// The blank embed import above enables this compiler directive.
//
//go:embed 001_ledger.sql
var initial string

//go:embed 002_reliability.sql
var reliability string

// Apply runs missing migrations and their version markers in one transaction.
// A failure rolls back the DDL and markers together; a restart can retry safely.
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
	// All application instances use the same advisory-lock number. This
	// transaction-scoped lock serializes simultaneous startup migrations and is
	// automatically released on commit/rollback, even after a client disconnect.
	if _, err = transaction.Exec(migrationContext, `SELECT pg_advisory_xact_lock(74209183)`); err != nil {
		return err
	}
	if _, err = transaction.Exec(migrationContext, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY)`); err != nil {
		return err
	}
	// Position maps to version 1, 2, ... . Add future migrations at the end;
	// changing executable SQL in an applied version would not upgrade existing DBs.
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
		// Only record a version after its SQL succeeds, inside the same transaction.
		if _, err = transaction.Exec(migrationContext, `INSERT INTO schema_migrations VALUES($1)`, version); err != nil {
			return err
		}
	}

	return transaction.Commit(migrationContext)
}
