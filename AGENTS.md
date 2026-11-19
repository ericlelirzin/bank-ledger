# Working on Wizardric

Wizardric is a personal banking portfolio demo built with React, TypeScript,
Go, and PostgreSQL. The dashboard opens as a fictional customer with a seeded
USD checking account. Transfers use the real ledger and a simulated bank.
Read [README.md](README.md) for setup and the current feature list.

## Where to start

- `web/src/App.tsx` contains dashboard navigation, payment forms, transaction
  details, and cash flow views. `web/src/bank.ts` is the API client and demo
  seeding logic. `web/vite.config.ts` proxies `/api` to Go in development.
- `internal/httpapi/banking.go` exposes account, recipient, and transfer routes.
  `internal/banking/banking.go` owns funding, reservation, and settlement.
  `internal/banking/history.go` handles history and timelines.
- `internal/ledger/ledger.go` owns double-entry posting. `migrations/` defines
  and enforces the database invariants. `cmd/ledger/main.go` starts the API or
  the settlement worker. `compose.yaml` runs those processes with PostgreSQL.
- For the transfer lifecycle, read [docs/banking.md](docs/banking.md). For ledger
  guarantees, read [docs/architecture.md](docs/architecture.md). For the raw
  ledger API, see [docs/ledger-reference.md](docs/ledger-reference.md).

## Follow one payment

Start with the send form in `App.tsx`, then follow `sendPayment` in `bank.ts`
through `POST /bank/transfers` to `CreateTransfer`. Creating a transfer and
reserving its funds share a transaction through `ledger.PostTx`. The worker
records a simulator receipt and then settles the transfer. The independent
receipt lets a retry recover if local settlement failed after bank confirmation.

Store USD amounts as integer cents. Preserve balanced, immutable journals and
the distinction between available and reserved funds. After an uncertain payment
response, retry the same payload and idempotency key. Use a forward migration
for schema changes so existing databases can upgrade.

## Verify changes

- Start the API, worker, and database with `docker compose up --build`. In
  `web/`, run `npm ci` and `npm run dev`, then open `http://localhost:5173`.
- For frontend changes, run `npm run build` in `web/` to type-check and bundle.
- For backend changes, run `go vet ./...` and `go test -race -count=1 ./...`.
  Set `TEST_DATABASE_URL` to a development PostgreSQL database to exercise the
  integration tests. Without it those tests skip. CI covers PostgreSQL 17 and 18.

## Current scope

Cards is a placeholder. Sample activity dates are illustrative. The local demo
stores its account reference in the browser. A public deployment needs
server-side account authorization, isolated visitor sessions, and private raw
ledger routes. The next planned milestone is a private AWS preview.

When explaining changes, start with what the customer sees, then name the
relevant Go, React, or PostgreSQL concept and point to the code or test.
