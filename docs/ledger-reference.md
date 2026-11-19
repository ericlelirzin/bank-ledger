# Ledger API and local configuration

This reference covers the accounting engine beneath the banking demo.
Start with the [project README](../README.md) for the simpler banking walkthrough.
The `/bank/...` product routes are described in [the banking guide](banking.md).

## Run Go locally

Requires Go 1.25+ and PostgreSQL 17+:

```sh
docker compose up -d db
export DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable'
go run ./cmd/ledger
```

`HTTP_ADDR` defaults to `:8080`. The following environment variables configure reliability limits; Compose forwards them with the same defaults:

| Variable | Default | Purpose |
| --- | --- | --- |
| `REQUEST_TIMEOUT` | `10s` | Whole HTTP handler deadline, including connection acquisition and posting |
| `DB_LOCK_TIMEOUT` | `2s` | Maximum PostgreSQL lock wait per acquisition |
| `DB_STATEMENT_TIMEOUT` | `5s` | Maximum PostgreSQL statement execution time |
| `DB_IDLE_TX_TIMEOUT` | `5s` | Terminates a session left idle in a transaction |
| `SHUTDOWN_TIMEOUT` | `10s` | Combined graceful drain and bounded pool cleanup budget |

Use Go duration syntax. Values must be between `1ms` and `1h`, with `DB_LOCK_TIMEOUT < DB_STATEMENT_TIMEOUT < REQUEST_TIMEOUT`. Half the shutdown budget is reserved for graceful requests; after that, handlers are cancelled and client connections closed. Pool cleanup gets the remaining half. A timeout never promises that an in-flight commit rolled back: retry an uncertain posting with the **same key and identical payload**.

To apply migrations without starting the API:

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

Successful requests return HTTP 200 and JSON. Errors use `{"error":"..."}`: 400 for malformed requests, 404 for missing accounts or journals, 409 for conflicting keys/account codes, 422 for database policy or balance violations, 503 for retryable transaction aborts or PostgreSQL timeouts, and 504 for a request deadline. Unexpected failures return 500 without database details. Unsupported routes and methods use Go's standard HTTP responses. Requests reject unknown fields and duplicate object keys at every nesting level, including escaped equivalents and case-folded aliases such as `amount`/`Amount`. This intentionally rejects ambiguous JSON accepted by v0.1. The 1 MiB request limit and integer JSON representation are unchanged.

Account fields are `code`, `name`, `kind`, `currency` (three uppercase letters), `normal_side` (`debit` or `credit`), and optional `allow_overdraft` (defaults to false). Account identity and policy cannot be edited in v0.2. Normal side is explicit to allow contra accounts; the usual convention is debit for assets/expenses and credit for liabilities/equity/income.

Amounts are **positive signed 64-bit integers in currency minor units**. No floating point or currency conversion is used. Currency exponent metadata is outside v0.2; callers must agree on the unit (for USD, cents). JSON clients must preserve 64-bit integers; JavaScript `Number` cannot represent all accepted values exactly. Balances increase on the account's normal side and decrease on its opposite side. Negative balances are allowed only when explicitly configured.

Keys are required, database-wide, shared across journal and transfer endpoints, and retained permanently. Request identity uses the decoded typed fields, including description and entry order, so JSON whitespace/property order does not affect replay. Entry reordering does change request identity. A failed transaction does not reserve its key. After any network error or uncertain commit outcome, retry the identical request with the **same key**; do not generate a new key.

