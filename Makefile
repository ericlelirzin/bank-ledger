# Phony targets are commands, even if a file happens to share their name.
.PHONY: test integration run bench
run:
	go run ./cmd/ledger
# Race detection checks Go memory access; database invariants need PostgreSQL tests.
test:
	go test -race ./...
# Fail explicitly if no test database was selected, rather than silently skipping.
integration:
	test -n "$$TEST_DATABASE_URL"
	go test -race -count=1 ./...

# Measure trigger validation work. This is a benchmark, not a throughput promise.
bench:
	test -n "$$TEST_DATABASE_URL"
	go test ./internal/ledger -run '^$$' -bench BenchmarkPostgresJournalValidation -benchtime=3x -count=1
