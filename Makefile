.PHONY: test integration run
run:
	go run ./cmd/ledger
test:
	go test -race ./...
integration:
	test -n "$$TEST_DATABASE_URL"
	go test -race -count=1 ./...
