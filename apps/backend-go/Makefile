# Run from apps/backend-go. Integration tests need TEST_DATABASE_URL pointing at a
# migrated Postgres 18 (owner credentials).
.PHONY: schema sqlc sqlc-check vet test test-integration test-all build-mcp

# opengtm-mcp: the MCP server (stdio, or `--sse [port]`).
build-mcp:
	go build -o ../../bin/opengtm-mcp ./cmd/mcp

schema:
	../../scripts/go_schema_dump.sh

sqlc:
	go tool sqlc generate

sqlc-check:
	go tool sqlc diff

vet:
	go vet ./...

# Unit tests, then integration tests when TEST_DATABASE_URL is set. Without it
# the integration suite is skipped with a loud warning (exit 0); use test-all to
# make that a failure.
test:
	go test -race ./...
	@if [ -n "$$TEST_DATABASE_URL" ]; then \
		$(MAKE) --no-print-directory test-integration; \
	else \
		echo "WARNING: TEST_DATABASE_URL is not set - INTEGRATION TESTS WERE SKIPPED (use 'make test-all' to require them)"; \
	fi

test-integration:
	go test -race -tags integration ./...

# Everything, or fail: needs TEST_DATABASE_URL and TEST_REDIS_URL.
test-all:
	@test -n "$$TEST_DATABASE_URL" || { echo "test-all: TEST_DATABASE_URL is not set" >&2; exit 1; }
	@test -n "$$TEST_REDIS_URL" || { echo "test-all: TEST_REDIS_URL is not set" >&2; exit 1; }
	go test -race ./...
	go test -race -tags integration ./...
