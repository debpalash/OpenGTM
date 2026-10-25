# Go executor parity harness

Release gate for the Go `run_workbook` executor: it proves the Go worker writes
exactly what the Python worker writes for the same workbook.

The harness lives with the (untracked) Python backend at
`legacy/python/tests/test_go_parity.py`, with fixtures in
`legacy/python/tests/fixtures/go_parity/*.json`. It is marked `postgres` and is
skipped when `TEST_DATABASE_URL` is unset or `go` is not on `PATH`.

## What it does

For each fixture (`happy`, `waterfall-fallback`, `no-match`, `no-match-free`,
`429`, `timeout`, `malformed`, `budget-cap`, `fill-missing`, `two-tenant`) it runs
the same workbook twice against a fresh `cmd/providersim` with the same
`SIM_SEED`: once through the real Python worker (`executor='python'`) and once
through the real Go worker binary (`executor='go'`, enqueued with
`go_executor.enqueue_run_workbook`). Workbook id, row ids and queue job id are
identical in both runs, so `attempt_key`, `run_id` and `contract_hash` are
compared byte for byte. It then diffs `workbook_enrichments`,
`workbook_rows.enrichments`, `workbook_spend_attempts`,
`workbooks.{status,completed_rows,budget_spent_usd}`, the job receipt and the
Redis messages published on `workbook:<id>`. Only wall-clock values and fresh
uuids are dropped (listed with reasons at the top of the test).

## How to run

Needs the test Postgres and Redis containers (`opengtm-go-test-pg` on 55480,
`opengtm-go-test-redis` on 56380) and a Go toolchain.

```sh
cd legacy/python
TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55480/opengtm_test \
TEST_REDIS_URL=redis://127.0.0.1:56380/0 \
uv run pytest -q -m postgres tests/test_go_parity.py
```

Set `PARITY_DUMP=<dir>` to write each side's normalized snapshot to
`<dir>/<fixture>.{python,go}.json` for diffing.

## Things to know

- Provider manifests are written under the Python manifests dir in `_parity_tmp/`
  for the run and removed afterwards. They point at the machine's own hostname:
  Go's manifest URL pre-check rejects loopback IPs and `localhost` even when
  allowlisted, so the simulator binds `0.0.0.0` and `EGRESS_ALLOW_HOSTS` carries
  the hostname.
- The Python worker runs with a small `sitecustomize` shim that fills in
  `EnrichmentResult.provider` for declarative providers. Stock Python leaves it
  empty, so every paid declarative lookup is recorded `accounting_uncertain`.
  See the report for details.
- Python reads a workbook's rows without `ORDER BY`; the harness runs
  `VACUUM FULL workbook_rows` between runs so heap order equals id order, which
  is what Go uses.
- Auth type `none` manifests only, so the internal credential endpoint is not
  exercised (the Go worker gets a dummy `INTERNAL_API_URL`).
