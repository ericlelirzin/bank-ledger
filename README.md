# bank-ledger

An educational, correctness-first double-entry ledger in Go and PostgreSQL.
Version 0.1 is a modular monolith: one Go process, one database, pgx, and handwritten SQL. It is not production banking software.

## What works

- Chart of accounts with asset, liability, equity, income, and expense accounts.
- Immutable journals with positive integer debit/credit entries; total debits must equal total credits in one currency.
- Atomic posting, transaction rollback, account balance queries, and internal transfers.
- Persistent idempotency keys, ordered account locks, overdraft checks, and overflow rejection.
- Embedded SQL migration, minimal JSON HTTP API, Docker Compose, and unit/PostgreSQL integration tests.

## Run with Docker Compose

Requires Docker with Compose v2:

```sh
docker compose up --build
```

The API listens on `http://localhost:8080`. PostgreSQL is available on `localhost:5432`. Both ports bind only to localhost. The application applies the migration at startup; database data persists in a named volume. The Compose credentials are local development credentials.

```sh
curl --fail http://localhost:8080/healthz
```

Stop with `docker compose down`; this preserves the database volume.

## Run Go locally

Requires Go 1.25+ and PostgreSQL 17+:

```sh
docker compose up -d db
export DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable'
go run ./cmd/ledger
```

`HTTP_ADDR` defaults to `:8080`. To apply migrations without starting the API:

```sh
go run ./cmd/ledger migrate
```

## Try a posting and transfer

These examples assume a fresh database, where the created account IDs are 1, 2, and 3. On an existing database, use the IDs returned by account creation. There is no preloaded money: opening balances require a balanced journal.

```sh
curl --fail-with-body localhost:8080/accounts -H 'Content-Type: application/json' -d \
  '{"code":"1000","name":"Bank cash","kind":"asset","currency":"USD","normal_side":"debit"}'
curl --fail-with-body localhost:8080/accounts -H 'Content-Type: application/json' -d \
  '{"code":"2001","name":"Alice deposit","kind":"liability","currency":"USD","normal_side":"credit"}'
curl --fail-with-body localhost:8080/accounts -H 'Content-Type: application/json' -d \
  '{"code":"2002","name":"Bob deposit","kind":"liability","currency":"USD","normal_side":"credit"}'

# Record an educational example of a $100 deposit: debit bank cash, credit Alice.
curl --fail-with-body localhost:8080/journals -H 'Content-Type: application/json' -d \
  '{"idempotency_key":"deposit-001","description":"Opening deposit","entries":[{"account_id":1,"side":"debit","amount":10000},{"account_id":2,"side":"credit","amount":10000}]}'

# Move $25 of the bank liability from Alice to Bob.
curl --fail-with-body localhost:8080/transfers -H 'Content-Type: application/json' -d \
  '{"idempotency_key":"transfer-001","from_account_id":2,"to_account_id":3,"amount":2500}'

curl --fail localhost:8080/accounts/2
curl --fail localhost:8080/accounts/3
curl --fail localhost:8080/journals/2
```

Alice's normal-side balance is `7500`; Bob's is `2500`. Repeating the identical transfer returns the same `journal_id` and `"replayed":true`. Changing its amount while reusing `transfer-001` returns HTTP 409.

## API

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/healthz` | Checks database connectivity |
| POST | `/accounts` | Creates a zero-balance account |
| GET | `/accounts` | Lists the chart of accounts with current balances |
| GET | `/accounts/{id}` | Reads account details and normal-side balance |
| POST | `/journals` | Posts 2–1000 balanced entries atomically |
| GET | `/journals/{id}` | Reads a committed journal and its entries |
| POST | `/transfers` | Posts a transfer between distinct accounts with matching currency and normal side |

Successful requests return HTTP 200 and JSON. Errors use `{"error":"..."}`: 400 for malformed requests, 404 for missing accounts or journals, 409 for conflicting keys/account codes, 422 for database policy or balance violations, and 503 for retryable transaction aborts. Unexpected failures return 500 without database details. Unsupported routes and methods use Go's standard HTTP responses.

Account fields are `code`, `name`, `kind`, `currency` (three uppercase letters), `normal_side` (`debit` or `credit`), and optional `allow_overdraft` (defaults to false). Account identity and policy cannot be edited in v0.1. Normal side is explicit to allow contra accounts; the usual convention is debit for assets/expenses and credit for liabilities/equity/income.

Amounts are **positive signed 64-bit integers in currency minor units**. No floating point or currency conversion is used. Currency exponent metadata is outside v0.1; callers must agree on the unit (for USD, cents). JSON clients must preserve 64-bit integers; JavaScript `Number` cannot represent all accepted values exactly. Balances increase on the account's normal side and decrease on its opposite side. Negative balances are allowed only when explicitly configured.

Keys are required, database-wide, shared across journal and transfer endpoints, and retained permanently. Request identity uses the decoded typed fields, including description and entry order, so JSON whitespace/property order does not affect replay. Entry reordering does change request identity. A failed transaction does not reserve its key. After any network error or uncertain commit outcome, retry the identical request with the **same key**; do not generate a new key.

## Test

```sh
go test -race ./...
```

Without `TEST_DATABASE_URL`, PostgreSQL integration tests explicitly skip. To run the complete suite:

```sh
docker compose up -d db
export TEST_DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable'
make integration
```

Integration tests create a unique schema per test, apply the migration twice to check repeatability, and drop only their own schemas afterward. The test role needs schema creation permissions. Use a development/test database. Tests cover concurrent spending, opposite-direction transfers, concurrent retries, payload conflicts, rollback, overflow, currency rules, immutable records, balance reconciliation, and direct SQL constraint enforcement. CI runs the suite against PostgreSQL 17 and 18.

See [architecture](docs/architecture.md) for the invariants, transaction protocol, and deliberate limitations. See [validation](docs/validation.md) for checks executed while building v0.1.

## Scope

No authentication, authorization, external payments, reconciliation service, holds, fees, FX, interest, account closure, pagination, or production operations tooling is included. Corrections use new compensating journals, never edits to posted entries. Run this educational API only in a trusted development environment.

Licensed under the [MIT License](LICENSE).
