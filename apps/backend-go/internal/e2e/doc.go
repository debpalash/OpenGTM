// Package e2e is the plan 15 "no-python" harness (WP-ALLGO). Its tests (build tag integration) build the real
// cmd/api and cmd/worker binaries, run them with GO_ALL_NATIVE=true against a private Postgres, a fake browser
// service and a fake LLM, and point PY_WORKER_URL and INTERNAL_API_URL at a server that records (and fails the
// test on) any request. Each scenario is a subtest; a scenario whose Go package has not landed is skipped with
// the package name, so the suite grows as packages land. See README in this file's tests for env requirements:
// TEST_DATABASE_URL (private clone of the test schema) and REDIS_E2E_URL (default redis://127.0.0.1:56380/2).
package e2e
