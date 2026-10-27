# Go executor parity harness

> **Historical (plan 15).** OpenGTM now runs without the Python backend: the Python api, worker, scheduler
> and seed services, the root `Dockerfile` and the nginx canary (`go-split.conf`, `go-routes.conf`) are gone, and
> every Go path is on (`GO_ALL_NATIVE=true` in compose). This document records the side-by-side period. Current
> operation: `docs/go-port/runbooks/docker-compose.md`; final cutover: `docs/go-port/runbooks/15-cutover.md`.

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

## What parity covers

- Same workbook, same simulator seed, concurrency 1: cells, row overlay, spend
  ledger (including `contract_hash`), budget, receipt and Redis broadcasts match.
- Replay: a job run by one executor, then reset (same job id, new lease) and run
  by the other, reuses the settled attempts: no new rows, same hashes, no
  provider calls, budget and cells unchanged (both directions).
- Safety: the harness refuses to run unless the database name ends in `_test`,
  and only deletes its own `wb_parity_*` workbooks, job ids 9000-9999 and its
  providers' `provider_stats`. It fails if other pending jobs exist, because a
  worker would claim them.

## Handed to the staging drill (not covered here)

- Concurrency greater than 1 (the simulator is call-index driven, so only
  serial runs are comparable).
- The credential endpoint: manifests use `auth: none`, so `INTERNAL_API_URL` is a
  dummy and no secret is fetched.
- A real SIGKILL crash and restart mid-run (only clean replay is tested).
- Python row-order nondeterminism: Python reads a workbook's rows with no
  `ORDER BY` (heap order); Go orders by id. The harness runs `VACUUM FULL
  workbook_rows` between runs so the two agree. In production, Python's row
  processing order is not stable.

## Open Go issue

`egress.CheckURL` (used by `manifest.Execute`) ignores the egress allowlist, so
loopback IPs and `localhost` are rejected even with `EGRESS_ALLOW_HOSTS` set in
test/bench, unlike Python's `check_url`. The harness works around it by pointing
manifests at the machine's own hostname with the simulator bound to `0.0.0.0`.
Needs a fix in `internal/egress` (not done here).

## Notes

- Provider manifests are written under the Python manifests dir in `_parity_tmp/`
  for the run and removed afterwards (a stale dir from a killed run is removed
  at the start).
- Python providers needed a one-line fix found by this harness:
  `DeclarativeProvider.enrich` now sets `provider=self.name` on every result;
  before, every paid declarative lookup was recorded `accounting_uncertain`.
