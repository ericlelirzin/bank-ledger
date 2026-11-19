# Banking accounts and transfers

A banking backend built with Go and PostgreSQL. Open a simulated business account,
add pretend money, and send a transfer to a fictional recipient.

The goal is to build a financial application that handles money carefully—and
makes it possible to understand what happened when something goes wrong.

**Current stage:** a simulated banking backend and a local personal banking
dashboard. All money is simulated. A public demo will require server-side access
controls and separate visitor sessions.

## What you can do today

- Open a USD business account and add simulated funds.
- Save recipients, called **beneficiaries**.
- Send transfers and follow their progress.
- See how much money is available and how much is reserved for pending transfers.
- View transfer history and the accounting records behind each transfer.

## Run it on your computer

Install Docker with Docker Compose, then open a terminal in this repository:

```sh
docker compose up --build
```

This starts three things:

| Part | What it does |
| --- | --- |
| API | Receives requests at `http://localhost:8080` |
| Database | Saves accounts, transfers, and accounting records |
| Worker | Picks up pending transfers and completes them through a simulated bank |

Keep that terminal open. In a second terminal, check that the API is ready:

```sh
curl --fail http://localhost:8080/healthz
```

Once startup finishes, you should see `{"status":"ok"}`. This is an API, so opening
its address in a browser will not show a banking dashboard.

## Try it: add $100, then send $25

Amounts are written in **cents**: `10000` means $100.00.
These commands use a Bash-compatible shell and `curl`.

### 1. Open an account

```sh
curl --fail-with-body http://localhost:8080/bank/accounts \
  -H 'Content-Type: application/json' \
  -d '{"name":"Wizard Works"}'
```

The response includes an `id`. Put that number below, replacing `1` if necessary.
Keep using the same terminal for the remaining steps:

```sh
ACCOUNT_ID=1
```

### 2. Add $100 of pretend money

```sh
curl --fail-with-body "http://localhost:8080/bank/accounts/$ACCOUNT_ID/simulated-funding" \
  -H 'Content-Type: application/json' \
  --data @- <<EOF_JSON
{"idempotency_key":"demo-funding-$ACCOUNT_ID","amount":10000}
EOF_JSON
```

An **idempotency key** identifies one operation. If you repeat this request with
the same key and details, the API will not add another $100. A new operation needs
a new key. Reusing a key with different details returns an error.

### 3. Send $25 to a fictional supplier

```sh
curl --fail-with-body http://localhost:8080/bank/transfers \
  -H 'Content-Type: application/json' \
  --data @- <<EOF_JSON
{"idempotency_key":"demo-payment-$ACCOUNT_ID","account_id":$ACCOUNT_ID,"destination":"SIM-SUPPLIER-001","amount":2500}
EOF_JSON
```

The transfer starts as `pending`. The worker then asks the simulated bank to
complete it, and the status changes to `settled`.

### 4. Check the result

```sh
curl --fail "http://localhost:8080/bank/accounts/$ACCOUNT_ID"
curl --fail "http://localhost:8080/bank/accounts/$ACCOUNT_ID/transfers"
```

After settlement, all balances are in cents:

| Field | Expected value | Meaning |
| --- | --- | --- |
| `posted_balance` | `7500` | $75 remains after the completed payment |
| `available_balance` | `7500` | $75 can be used for another transfer |
| `reserved_balance` | `0` | Nothing is waiting to be sent |

If the worker has not finished yet, you may briefly see $100 posted, $75
available, and $25 reserved. Run the commands again after a moment.

To inspect a transfer's timeline, take its `id` from the response and request
`GET /bank/transfers/{id}/timeline`. It shows when the transfer was created,
funds were reserved, the simulated bank confirmed it, and settlement completed.

## Stop it

Press Ctrl+C in the first terminal, then run:

```sh
docker compose down
```

Your data stays saved for the next run.

## How the money is tracked

The **ledger** is the accounting engine underneath the product. Each movement
creates a balanced accounting record: the amount leaving one place matches the
amount entering another. Saved records cannot be edited through normal database
writes; a correction needs a new record.

When you send a transfer, its money moves from available to reserved. This stops
another transfer from spending the same funds. When settlement finishes, the
reserved funds leave the account.

If processing stops after the simulated bank confirms a payment, the worker can
retry without paying twice. Tests exercise this failure and check the balances.

## Run the tests

For local Go development, install Go 1.25 or later. Run:

```sh
go test -race ./...
```

That command skips database tests unless `TEST_DATABASE_URL` is set. To include
them, start the development database and run:

```sh
docker compose up -d db
export TEST_DATABASE_URL='postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable'
go test -race -count=1 -timeout=2m ./...
```

Use a development database. Each integration test creates and removes its own
isolated database schema. Tests cover competing transfers, repeated requests,
failed settlement recovery, balances, and database upgrades. CI tests against
PostgreSQL 17 and 18.

## Explore the implementation

| Location | What to look for |
| --- | --- |
| `internal/banking/` | Business accounts, recipients, transfers, and timelines |
| `internal/ledger/` | Accounting rules and posting transactions |
| `internal/httpapi/` | API routes and request validation |
| `cmd/ledger/` | Starting the API or worker |
| `migrations/` | Changes to the database structure |

For more detail:

- [Banking guide](docs/banking.md): recipients, timelines, and transfer processing.
- [Ledger reference](docs/ledger-reference.md): accounting API and local configuration.
- [Architecture](docs/architecture.md): transaction rules and failure handling.
- [Earlier ledger validation](docs/validation.md): checks and measurements from v0.2.

## Run the personal banking dashboard

The React and TypeScript dashboard lives in `web/`. Start the API, PostgreSQL,
and the settlement worker with `docker compose up --build`. Then start Vite
using Node.js 20.19+ or 22.12+:

```sh
cd web
npm ci
npm run dev
```

Open `http://localhost:5173`. The first visit creates an Everyday demo account,
posts two sample deposits, and reserves four sample payments through the Go API.
The worker settles the transfers. Sample activity dates are illustrative, while
journal and timeline details come from the ledger. The browser remembers the
demo account locally. Go still validates and posts every payment.

Run `npm run build` in `web/` to type-check and build the frontend. Set
`BANK_API_URL` if the local API is listening somewhere other than
`http://127.0.0.1:8080`. Keep this dashboard in local development. The demo
account reference is stored in the browser and the existing API requires server-side
authorization and visitor isolation before a public deployment.

Licensed under the [MIT License](LICENSE).
