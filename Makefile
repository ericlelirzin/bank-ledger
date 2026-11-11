.PHONY: test integration run bench
run:
	go run ./cmd/ledger
test:
	go test -race ./...
integration:
	test -n "$$TEST_DATABASE_URL"
	go test -race -count=1 ./...

bench:
	test -n "$$TEST_DATABASE_URL"
	go test ./internal/ledger -run '^$$' -bench BenchmarkPostgresJournalValidation -benchtime=3x -count=1
