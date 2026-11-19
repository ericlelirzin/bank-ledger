# Simulated accounts and outgoing transfers: first slice

This is a local development backend for a future public banking demo. It uses
fictional USD transfers; it does not connect to a financial institution. Existing
ledger routes are retained for development. There is no authentication or tenant
isolation yet. Do not expose these mutation routes as a public demo.

## Run

`docker compose up --build` starts PostgreSQL, the API, and a transfer worker.
Alternatively, set DATABASE_URL and run `go run ./cmd/ledger` and
`go run ./cmd/ledger worker` in separate terminals. Both processes apply migrations
safely at startup. The worker processes at most one transfer every 500 ms.

For an inspectable pending transfer, start just the database and API first:

```sh
docker compose up --build db app
```

In another terminal, using IDs returned by each response:

```sh
curl localhost:8080/bank/accounts -H 'Content-Type: application/json' \
  -d '{"name":"Wizard Works"}'

curl localhost:8080/bank/accounts/1/simulated-funding -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"fund-demo-1","amount":1000000}'

curl localhost:8080/bank/transfers -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"supplier-demo-1","account_id":1,"destination":"SIM-SUPPLIER-001","amount":120000}'

curl localhost:8080/bank/accounts/1
curl localhost:8080/bank/transfers/1
```

Expected balances before processing: posted 1000000, available 880000, reserved
120000. Start `docker compose up --build worker` to settle. After processing:
posted 880000, available 880000, reserved 0. Read the transfer again to see its
reservation and settlement journal IDs. Existing GET /journals/{id} shows entries.
Repeated funding/transfer requests with the same key and payload have no second
financial effect. Changed payloads using an existing key return 409. Funding keys
and outgoing-transfer keys are separate namespaces; funding keys are global across
product accounts, as are outgoing-transfer keys. Account creation is not yet
idempotent. All amounts are integer USD cents.

## Accounting decision

Each product account owns three ledger accounts: simulated cash (debit-normal
asset), available customer funds (credit-normal liability), and reserved customer
funds (credit-normal liability). These are internal mappings, not beneficiary
accounts. Cash is tracked per product account for this initial simulator; a real
pooled-bank model would need explicit settlement and reconciliation accounts.

| Action | Debit | Credit |
| --- | --- | --- |
| Simulated inbound funding | Cash | Available funds |
| Reserve outgoing transfer | Available funds | Reserved funds |
| Settle outgoing transfer | Reserved funds | Cash |

Posted balance is the cash balance; available and reserved are the two liability
balances. For this closed model, posted = available + reserved. A balance response
reads all three in one SQL statement. The existing nonnegative account constraints
and common account lock ordering serialize competing reservations. There is no
separate unlocked balance check and no floating-point arithmetic.

Transfer creation, its reservation journal, and durable pending work commit in
one transaction using ledger.PostTx. The transfer row itself is the durable job;
a second outbox table is unnecessary for this polling worker. PostTx deliberately
does not commit: its caller must roll back errors and report success only after
commit and deferred database validation.

## Simulator and retry semantics

The worker locks one pending transfer with FOR UPDATE SKIP LOCKED. It calls the
simulator on a separate database connection. The simulator commits an immutable
receipt keyed by transfer ID and checks identical destination/amount on replay.
Only then does the worker post settlement and mark the transfer settled atomically.

If the process fails after receipt creation, the transfer remains pending and
funds remain reserved. Retry finds the receipt and completes local settlement
without a second external effect. A settlement commit with a lost acknowledgment
is likewise safe: the next worker scan only selects pending transfers. Multiple
workers can operate concurrently. Each worker needs one connection for its claim
and another for the simulator; provision the pool with spare connections.

This is a deterministic local stand-in for an idempotent bank API. It confirms
settlement immediately; it does not yet model acceptance separately from final
settlement, real webhooks, returns, or provider outages. Shared PostgreSQL storage
makes it unsuitable for claiming real network/crash durability guarantees.
Persistent processing failures currently log and retry on the polling interval;
backoff, attempt visibility, and operator recovery remain follow-up work.

## Verification and next milestones

The banking integration tests exercise migrations applied twice, funding and
transfer replay/conflicts, concurrent insufficient-funds reservations, journal
balance reconciliation, and recovery after a real simulator receipt commits but
local settlement is forced to roll back. Concurrent retrying workers must produce
one receipt and one financial effect. Run with TEST_DATABASE_URL and
`go test -race -count=1 -timeout=2m ./...`.

The [local dashboard](../README.md#run-the-personal-banking-dashboard) now
uses these routes. The next planned deployment is a private AWS preview. A
public demo needs server-side authorization, isolated visitor sessions, and
private low-level ledger routes. Independent reconciliation and richer provider
events are future work.

## Beneficiaries, history, and timelines

Migration 004 adds immutable, account-scoped beneficiaries. To create one:

```sh
curl localhost:8080/bank/accounts/1/beneficiaries -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"supplier-001","name":"Demo supplier","destination":"SIM-SUPPLIER-001"}'

curl localhost:8080/bank/transfers -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"pay-supplier-001","account_id":1,"beneficiary_id":1,"amount":120000}'

curl 'localhost:8080/bank/accounts/1/beneficiaries?limit=20'
curl 'localhost:8080/bank/accounts/1/transfers?limit=20'
curl localhost:8080/bank/transfers/1/timeline
```

Substitute the IDs returned by the API. A transfer accepts either beneficiary_id
or a nonempty destination, never both. Direct destinations remain supported for
existing development clients. A beneficiary must belong to the source account;
a composite foreign key independently enforces this association. The transfer
stores its resolved destination and beneficiary reference. Changing the reference
to a raw destination with the same string is still a conflicting replay.
Beneficiary creation keys are scoped to the account. Replaying the same key and
fields returns the original beneficiary; changed fields return 409. Editing and
deleting beneficiaries are intentionally unsupported in this slice.

Both list endpoints return `{"items":[...],"next_before_id":123}`. Set
`before_id` to that cursor to continue; null means there are no more results.
Default limit is 50, maximum 100. Results use descending identity order, not
settlement or commit order. Lists are live queries, not snapshots spanning pages;
concurrently committed inserts may require refreshing the first page. Empty lists
return an empty array. Unknown product accounts return 404.

A timeline returns transfer_created, funds_reserved, bank_confirmed, and
transfer_settled when each fact exists. Reservation and settlement events include
journal_id; confirmation includes provider_reference (the simulator's transfer
ID). Events use logical lifecycle order; timestamps describe record creation and
are not a global commit ordering. All facts are read in one database statement.
A receipt committed before a failed settlement remains visible as bank_confirmed;
the timeline does not invent a settled event or release reserved funds. Retries
do not add duplicate events. This is a projection of committed facts, not a full
attempt log or an independent audit export.

Additional tests cover beneficiary isolation and replay, destination snapshots,
both paginated lists, intermediate timelines during failed settlement, and
migration of a populated version-003 database followed by successful settlement.
