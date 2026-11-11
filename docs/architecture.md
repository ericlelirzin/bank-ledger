# Architecture and financial invariants

## Modules and storage

`cmd/ledger` owns startup, configuration, migration, HTTP timeouts, and graceful shutdown. `internal/httpapi` decodes bounded JSON requests and maps results to HTTP. `internal/ledger` owns account operations, posting, transfers, and queries. `migrations` embeds the handwritten SQL schema and applies it inside a transaction under a PostgreSQL advisory lock. There is one application and one database; no ORM or message broker.

The storage model has three main tables:

- `accounts`: unique chart code, classification, currency, normal side, overdraft policy, and current balance.
- `journals`: immutable posting header, globally unique idempotency key, SHA-256 request hash, timestamp, and inserting PostgreSQL transaction ID.
- `entries`: immutable journal lines, account references, side, and positive `bigint` amount.
- `pending_journals`: an internal transaction-local work queue for deferred validation; normally empty outside an active posting.

Entries are the accounting record. `accounts.balance` is a synchronous projection maintained by a database trigger, not a second independent source of money. It commits or rolls back with the entries. Identity columns may have gaps on rollback; neither journal IDs nor creation timestamps imply commit order.

## Invariants enforced by PostgreSQL

Every committed journal has at least two entries, exactly one currency, and equal debit and credit totals. Journal and entry insertion mark the journal in `pending_journals`, using its primary key to deduplicate marks. A deferred constraint trigger validates the full entries and removes the marker. Totals use PostgreSQL `numeric`, avoiding overflow during summation.

The v0.1 design scheduled a full aggregate for the header and every line, scanning `n` entries `n+1` times. Migration 002 replaces it with one full aggregate per dirty journal between constraint flushes. Each line still incurs a constant-size queue lookup. The balances and journal entries remain the accounting data; there is no new running-total projection to trust.

If SQL forces `SET CONSTRAINTS ALL IMMEDIATE`, validation consumes the marker. Subsequent entries insert a fresh marker, so toggling constraints back to deferred cannot hide an unbalanced append. Savepoint rollback restores the marker and its pending validation together. Ordinary direct INSERT/UPDATE/DELETE of queue rows is rejected. Skipped entry inserts never mark a journal or alter balances. A successful commit leaves no queue rows. A constraint flush after every small statement can still rescan a growing journal; this is necessary to enforce immediate validation. Tests cover this behavior, queue protection, late appends, and savepoint rollback.

Journal headers and entries reject updates and deletes. Entry insertion requires that the parent journal was inserted in the current database transaction (`xid8`), preventing later append operations. A posting is completed by committing its inserting transaction; there is no persisted draft state.

Entry triggers update balances **after successful insertion**. A skipped `INSERT ... ON CONFLICT DO NOTHING` must not change money. All accounts start at zero, and ordinary direct balance updates are rejected. Account identity, currency, normal side, and overdraft policy are fixed in v0.1, so historical interpretation cannot silently change.

Amounts are positive `bigint`; balances are signed `bigint`. Database arithmetic rejects overflow. Account checks prevent negative normal-side balances unless overdraft is enabled. Constraints and triggers apply to ordinary SQL writes as well as application writes. A database owner or superuser can disable triggers, change functions, or truncate tables; this schema is not a security boundary against a privileged database administrator. The development app role also owns migrations; a production deployment would separate ownership and runtime privileges.

## Posting protocol

1. Validate the request, including positive amounts, valid sides, entry count, and balanced arbitrary-precision totals in Go. Copy the entries so posting never mutates a caller's request.
2. Begin a PostgreSQL transaction at the default `READ COMMITTED` isolation level.
3. Insert a journal with its unique idempotency key and a hash of a typed, operation-tagged JSON representation. PostgreSQL's unique index waits for concurrent attempts using that key. If the committed key already exists, compare hashes and return its journal ID, or reject a conflicting payload. A concurrent failed attempt releases the key through rollback.
4. Lock every affected account with `SELECT ... FOR UPDATE`, one at a time in ascending account ID order. Validate existence and matching currency while holding these locks. Every application posting follows this order, including opposing transfers.
5. For a transfer, derive two sides: decrease the source's normal balance and increase the destination's normal balance. This convenience operation requires matching normal sides. General journals support accounts with different normal sides.
6. Insert entries with normal-side increases first, avoiding transient overdrafts when an account occurs more than once in a journal. Entry triggers atomically maintain balances; policy or overflow failures abort the transaction.
7. Commit only when deferred journal checks pass. Rollback on every error or cancellation. Return success only after commit succeeds.

`READ COMMITTED` is sufficient because every account affecting the invariant is explicitly locked until commit; funds decisions never rely on an unlocked read. The database's balance update and check constraint independently reject negative balances. Public balance queries return committed values, but the value may change immediately afterward; callers must use posting operations to spend funds.

Deadlocks caused by ordinary application postings are avoided through a common account lock order. Direct SQL writers following a different lock order can still deadlock; PostgreSQL aborts one transaction. The API maps deadlocks, serialization failures, lock/statement timeouts, and idle-transaction timeouts to 503 and instructs callers to retry the same key. Request deadlines return 504; cancellation returns 503 when a response can still be delivered. There is no automatic transaction retry loop. Request cancellation, deadline expiry, and connection/commit errors may have an uncertain outcome; the same-key retry protocol resolves whether the journal committed.

## Accounting conventions and limitations

An account's balance is the sum of its normal-side entries minus opposite-side entries. For customer deposit liabilities, credits increase customer funds and debits decrease them. Opening balances must be journaled against another account. The schema permits explicit contra-account normal sides instead of deriving side from classification.

Each journal uses one currency; there is no FX netting. Currency identifiers have a three-uppercase-letter syntax check, not a registry lookup. Minor-unit conventions belong to the caller. Journals can include multiple lines for an account or a balanced zero-net movement; the convenience transfer endpoint rejects a self-transfer.

Increases are inserted before decreases. Consequently an intermediate balance that exceeds the signed 64-bit range is rejected even if later decreases would bring its final value into range. This conservative bound avoids wrapping or accepting an unrepresentable balance. A multi-line journal near the limit may need to be expressed differently.

`GET /journals/{id}` can read header and entries in separate statements safely because committed journals cannot change. `GET /accounts` reads its list in a single SQL statement. Separate HTTP balance requests do not constitute a consistent multi-account snapshot.

The migration runner contains immutable v0.1 migration 001 and forward reliability migration 002, with one version marker per applied migration. It serializes concurrent startup, runs DDL atomically, and skips the applied version on restart. Future changes must add new forward migrations; never edit an applied migration. There is deliberately no destructive automatic down migration.

## Deadlines and shutdown

HTTP handlers derive an explicit deadline from their request context. The Go deadline covers pool acquisition and the entire posting protocol, rather than just one SQL statement. Each application database connection is configured with `lock_timeout`, `statement_timeout`, and `idle_in_transaction_session_timeout`. Connections also have a bounded connect timeout. PostgreSQL limits protect server resources when a client cannot deliver cancellation; they do not replace the whole-request deadline. Transaction cleanup uses an independent two-second context, because the caller's context may already be cancelled.

The runtime configures all pool connections, including migration connections. Startup migrations additionally have a 30-second context; a configured statement or lock timeout may stop them earlier. Direct users of the internal ledger package remain responsible for supplying a context and configuring their pool.

On SIGINT/SIGTERM the listener stops accepting work and the process allows active handlers half of `SHUTDOWN_TIMEOUT` to finish. At the end of this grace period it cancels their base context and closes active HTTP connections. Pool closure waits at most the remaining half of the budget. The process can exit even if a peer or checked-out connection does not cooperate. Shutdown does not turn an uncertain commit into a definite rollback. Completed commits remain discoverable by their idempotency key.

## JSON and failure verification

Before typed decoding, the bounded JSON body is walked recursively to reject duplicate object keys. The walk enforces the same 10,000-container nesting limit as Go's JSON decoder. Unicode simple folding matches Go's case-insensitive field matching, including escaped keys and aliases such as `side`/`ſide`. Typed decoding then rejects unknown fields and preserves integer amounts. Requests accepted by v0.1 that relied on duplicate keys now receive HTTP 400; endpoint paths and successful response formats are unchanged.

Automated tests control database wait states before cancelling requests or rolling back a contested key holder. A test-only plaintext connection wrapper consumes a real successful PostgreSQL COMMIT response and then returns a transport error to pgx; retry must discover the original journal. This verifies recovery from a lost acknowledgment without adding fault hooks to production posting code. It does not simulate every network condition, TLS proxy behavior, server crashes, or WAL durability/recovery. These remain external fault-injection experiments.

## What v0.2 does not provide

This is an educational ledger, not a regulated banking system. It lacks access controls, payment settlement, holds/available-balance distinctions, durable external audit export, backups/recovery procedures, fraud controls, account pagination, and operational guarantees. API callers currently have full ability to create accounts and post general journals. Tests validate ledger invariants within this scope, not suitability for handling real deposits.
