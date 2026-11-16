# Reading sessions: from Go to committed ledger data

The inline comments explain each meaningful block: what it does, which failure
it prevents, and which other file supplies the rest of the guarantee. Follow the
sessions in order, or choose one question and trace it through code and tests.

## 1. What exactly is the request?

Read the types, `Validate`, `Post`, and `Transfer` in
[`ledger.go`](../internal/ledger/ledger.go).

Follow `Posting.Key`, the entry slice, positive `int64` amounts, and the explicit
normal side. A transfer constructs a posting with unset sides; account metadata
under lock supplies those sides later. `big.Int` handles totals larger than an
individual amount can represent.

Read `TestValidate` in [`ledger_test.go`](../internal/ledger/ledger_test.go).
Check why several individually valid amounts can still overflow a naive sum.

## 2. How does a request become one atomic database operation?

Read `post` in [`posting.go`](../internal/ledger/posting.go) from top to bottom.

Track the one `transaction` variable through claim, locks, inserts, and commit.
Each early return triggers deferred rollback. The fresh cleanup context exists
because the request context may already be cancelled. Successful inserts are
still provisional: commit runs deferred checks and can fail.

Follow the entry insert into `apply_entry` in
[`001_ledger.sql`](../migrations/001_ledger.sql). The balance update happens in
this SQL trigger, inside the same transaction as the inserted entry.

Read `TestPostgresCancellationAfterAppliedEntry` in
[`reliability_test.go`](../internal/ledger/reliability_test.go).
The test cancels after some transactional work happened, then checks rollback.

## 3. What if concurrent requests or retries arrive?

Read `requestHash`, `claimJournal`, and `lockAccounts` in `posting.go`.

The journal's unique key arbitrates duplicate requests. The digest compares
payload identity. Account row locks coordinate spending across processes.
Ascending lock order prevents ordinary application postings from forming a
circular account-lock wait. These are three distinct responsibilities.

Read `TestPostgresConcurrency` and `TestPostgresOpposingTransfers` in
[`integration_test.go`](../internal/ledger/integration_test.go), then the key-holder
rollback and conflicting-key tests in `reliability_test.go`.

Read [`ack_failure_test.go`](../internal/ledger/ack_failure_test.go) last. Its
connection wrapper consumes a real successful COMMIT response while returning a
transport error to pgx. Same-key retry resolves the uncertain outcome. A Go error
alone cannot prove that PostgreSQL rolled back.

## 4. Which guarantees belong to PostgreSQL?

Read `001_ledger.sql`, then [`002_reliability.sql`](../migrations/002_reliability.sql).
Read both: 002 replaces the original repeated full-journal validation triggers.

Trace an entry through balance update, account CHECK constraints, pending marker,
and deferred journal validation. The marker table normally ends every successful
transaction empty. It is a transactional deduplication mechanism, not a worker
queue. Early `SET CONSTRAINTS` checks and savepoint rollback explain its lifecycle.

Read the database-guard tests in `integration_test.go`, followed by the isolated
invariant, savepoint, and validation-count tests in `reliability_test.go`.

The historical SQL files contain added teaching comments; their executable
statements are unchanged. Future schema behavior changes need a new migration.
Trigger enforcement assumes trusted schema ownership. Durability and recovery
also depend on PostgreSQL settings, storage, backups, and recovery procedures;
these tests do not establish protection against every machine or storage failure.

## 5. What happens before SQL and after commit?

Read [`http.go`](../internal/httpapi/http.go): `Handler`, `decode`, `uniqueKeys`,
then `respond`. Follow JSON into typed structs and errors back into HTTP status
codes. Duplicate keys are rejected before request identity is established.

Read the adjacent HTTP tests. Notice that writing a response happens after a
successful posting: a failed network write cannot undo committed accounting data.
Keep the original request and key for retries. Stored journal lines may have been
reordered for safe insertion and do not necessarily reconstruct request order.

## 6. How does it stay bounded under load and shutdown?

Read [`config.go`](../cmd/ledger/config.go), [`main.go`](../cmd/ledger/main.go),
[`migrate.go`](../migrations/migrate.go), then [`server.go`](../cmd/ledger/server.go).

Distinguish the connection pool, per-lock timeout, per-statement timeout,
whole-request deadline, and shutdown grace period. Migrations acquire an advisory
lock so simultaneous application starts do not race their schema changes.

Read [`server_test.go`](../cmd/ledger/server_test.go). Tests observe actual SQL wait
states before injecting cancellation rather than guessing when a request blocked.
The bounded waits prevent indefinite waiting; they are not a throughput guarantee.
Each account lock and entry insert currently takes a separate database call, and
shared accounts serialize competing postings.

## Go notation used repeatedly

| Code | Meaning here |
| --- | --- |
| `value, err := operation()` | Declare results locally; an error stops this protocol step. |
| `if err != nil { return ... }` | Propagate failure and let deferred cleanup run. |
| `*Transfer` / `nil` | An optional transfer pointer; nil selects explicit journal handling. |
| `&account.ID` | Address where pgx Scan writes the SQL column value. |
| `[]Entry` | A slice; copying its enclosing struct still shares its backing array. |
| `map[int64]Account` | Local lookup by account ID; not another SQL query. |
| `func(...) bool { ... }` | A callback; sorting calls it to compare two entries. |
| `defer cleanup()` | Run cleanup when this function returns, including error returns. |
| `context.Context` | Carries cancellation/deadlines; it is not a database transaction. |
| `go func() { ... }()` | Start concurrent Go work; it does not make SQL writes atomic. |
| `channel <- value` / `<-channel` | Send/receive between goroutines; used to coordinate tests and shutdown. |
| `errors.Is` / `errors.As` | Recognize error categories or extract structured error details. |

For each session, ask: which state can change, which operation protects it, when
does that protection end, and which test would fail if it were removed?
