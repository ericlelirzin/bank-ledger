# v0.1 validation

Validated in the implementation environment using Go 1.25.4 and a temporary PostgreSQL 18.6 instance on localhost.

## Executed successfully

- `go test -race -count=1 ./...` with `TEST_DATABASE_URL` set: all unit, ledger integration, and HTTP integration tests passed. PostgreSQL tests were executed, not skipped.
- `go vet ./...`: passed.
- `go build -trimpath -o /tmp/bank-ledger ./cmd/ledger`: passed, including a `CGO_ENABLED=0` static build matching the Docker build configuration.

The PostgreSQL tests use isolated schemas and cover:

- Migrations on a new schema and repeat application.
- Balanced posting, normal-side balances, journal reads, and balance reconciliation.
- Same-payload replay and conflicting-payload rejection.
- Forty concurrent spend attempts against limited funds, with exactly ten successes.
- Twenty simultaneous retries producing one journal.
- Forty opposing transfers without deadlocks or lost updates.
- Overdraft failure, integer overflow, rollback, cancelled requests, and reuse of failed keys.
- Rejection of mixed currencies, empty/unbalanced journals, mutable entries/headers, late appends, and direct balance changes.
- Regression checks for skipped inserts under `ON CONFLICT DO NOTHING` and appending entries after early deferred-constraint validation.
- HTTP account creation/listing, liability-account funding and transfers, replay, journal retrieval, error status codes, and integer JSON decoding.

## Environment limitations

Docker is unavailable in this WSL distribution because Docker Desktop integration is not enabled. Consequently the Docker image build, Compose startup, and PostgreSQL 17 container could not be executed here. A downloaded PostgreSQL 18.6 server was used for database validation instead; it required permission to open local sockets. No system PostgreSQL installation was changed.

The repository includes CI jobs for PostgreSQL 17 and 18, but those remote jobs have not been executed from this workspace. Run `docker compose up --build` on a machine with Docker available to verify the packaged startup.
