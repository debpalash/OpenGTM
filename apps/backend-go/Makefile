# Run from apps/backend-go. Integration tests need TEST_DATABASE_URL pointing at a
# migrated Postgres 18 (owner credentials).
.PHONY: schema sqlc sqlc-check vet test test-integration

schema:
	../../scripts/go_schema_dump.sh

sqlc:
	go tool sqlc generate

sqlc-check:
	go tool sqlc diff

vet:
	go vet ./...

test:
	go test -race ./...

test-integration:
	go test -race -tags integration ./...
