# v0.2 reliability validation

The reliability work is based on commit `a505cea` and uses Go 1.25.4 and a temporary PostgreSQL 18.6 instance on localhost. Tests create isolated schemas and remove them after completion. The unchanged v0.1 SQL is also exercised as an upgrade fixture and benchmark baseline.

## Automated checks executed successfully

- `go test ./...`: unit tests passed; integration tests explicitly skip when `TEST_DATABASE_URL` is absent.
- `go test -race -count=1 ./...` with `TEST_DATABASE_URL` set: unit, PostgreSQL, real HTTP, and deterministic fault tests passed.
- `go vet ./...`: passed.
- `CGO_ENABLED=0 go build -trimpath -o /tmp/bank-ledger-v02 ./cmd/ledger`: passed.

The suite verifies the existing v0.1 posting/concurrency behavior plus:

- Positive, consistent timeout configuration and PostgreSQL connection timeout settings.
- Real account-lock timeouts, statement timeouts, idle transaction termination, and whole-request deadlines.
- Client cancellation while waiting for account locks, pool connection release, graceful completion, forced shutdown, and bounded pool closure even with a checked-out connection.
- Cancellation after an earlier entry and balance update were successfully applied; no entry, journal, or balance change survives rollback, and the failed key can be reused.
- Concurrent same-key waiting with initial-holder rollback, waiter cancellation, and conflicting payloads.
- A real successful COMMIT whose acknowledgment is hidden from pgx by a test-only connection wrapper; the caller sees an error and the same-key retry returns the original journal without moving funds again. This wire test requires `sslmode=disable` and explicitly skips TLS connections.
- Duplicate JSON keys at every nesting level, escaped spellings, Unicode/case aliases, independent object scopes, unknown nested fields, excessive nesting, and rejection before any database access.
- Forward migration of a populated v0.1 schema, repeat migration application, preserved balances, and subsequent posting.
- Balanced late appends rejected specifically by the immutability guard; balanced mixed-currency journals rejected by PostgreSQL; unequal totals rejected with multiple otherwise valid lines; positive amount, side, and account-reference constraints checked individually.
- Queue tampering rejected, queue empty after commit, one validation per dirty journal between flushes, revalidation after early constraint flush, and marker restoration after savepoint rollback.
- Overflow after a preceding successful entry (the original v0.1 test order was corrected), failed-key reuse, valid aggregate totals above int64, exact MinInt64 balances on overdraft-enabled accounts, negative underflow rollback, and conservative rejection of intermediate overflow even when net movement is zero.

## Built-process shutdown probe

A separate local probe started the built binary, held an account row lock in an external PostgreSQL transaction, waited until an HTTP transfer was blocked, and sent SIGTERM. With `SHUTDOWN_TIMEOUT=600ms`, the process exited successfully in approximately 316 ms while the external lock remained held. After releasing the lock, the attempted journal count was zero. The same probe confirmed duplicate amount fields return HTTP 400 and leave balances unchanged. This manual process check supplements the checked-in real HTTP and pool cleanup tests.

## Deferred validation benchmark

Run against the same PostgreSQL instance:

```sh
export TEST_DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable'
make bench
```

`BenchmarkPostgresJournalValidation` uses bulk SQL to isolate trigger work from Go's per-entry network round trips. Every journal is balanced with a zero-net movement on a funded account. Schema creation and account setup are outside the timer.

An illustrative local run (`-benchtime=3x`, no race instrumentation in the benchmark):

| Schema | Lines | Time per journal |
| --- | ---: | ---: |
| v0.1 | 100 | 8.35 ms |
| v0.1 | 1,000 | 399.86 ms |
| v0.2 | 100 | 4.48 ms |
| v0.2 | 1,000 | 70.77 ms |

At 1,000 lines this run was about 5.6 times faster. These short, hardware-dependent measurements include entry inserts, repeated account balance updates, queue lookups, and commit; they are not a throughput guarantee or evidence that all posting work is linear. A separate instrumented SQL regression proves that the full aggregate executes once per dirty journal between constraint flushes, rather than once per line. Forcing immediate constraints repeatedly retains the necessary repeated-validation cost.

## Limits and unverified experiments

Docker Desktop integration remains unavailable in this WSL distribution. The Docker image build, Compose startup, and PostgreSQL 17 container were not executed locally. The existing CI matrix covers PostgreSQL 17 and 18, but remote CI has not been run from this workspace.

The deterministic acknowledgment test proves one precise ambiguity: PostgreSQL committed, the driver did not receive success, and retry recovers the single journal. External proxies dropping traffic at arbitrary byte offsets, TLS acknowledgment loss, network blackholes, PostgreSQL process crashes/restarts, filesystem/WAL failure, and recovery from a machine power loss remain unverified. PostgreSQL's default durability settings and a trusted schema owner remain assumptions. No claim of production banking readiness is made.

## Deferred validation follow-up (v0.2)

Before treating the ledger as production-ready, independently verify:
- SET CONSTRAINTS IMMEDIATE/DEFERRED transitions and repeated journal inserts.
- Pending validation queue cleanup after successful commits.
- Direct SQL and nested-trigger attempts to manipulate pending_journals.
- Concurrent transaction behavior under PostgreSQL MVCC.
- Docker Compose execution and PostgreSQL 17 compatibility.
- Crash recovery, durability, and ambiguous commit outcomes.

Status: Follow-up audit pending. v0.2 remains educational software,
not a production-certified financial ledger.
