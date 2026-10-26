# Runbook: workbook CRUD cutover to Go (plan 04)

Go serves workbook CRUD, columns, rows, import/export, saved views, telemetry, templates, functions,
entities, analytics, `me/context`, `flags` and the files/system routes next to FastAPI, using the same
canary split as plans 02/03 (`$auth_upstream`, `infra/nginx/go-split.conf`). The 14 enrichment/LLM/Redis
workbook routes and the 2 ingest routes **stay on Python** and nginx splits per route.

## Routing summary

The plan-04 locations live in one delimited section at the end of `infra/nginx/go-routes.conf`
(`BEGIN/END plan 04`). Regex locations are tried in file order, first match wins, so the retained block
must stay above the Go block.

| Path | Upstream |
| --- | --- |
| `~ ^/api/workbooks/(meta/provider-(stats\|accuracy)\|{id}/(run(/estimate)?\|stop\|sources(/.*)?\|refresh(-policy)?\|migrate\|ws)\|{id}/rows/{row}/cells/{col}/run)` | always Python (no `X-Upstream` header) |
| `~ ^/api/v2/workbooks/{id}/(generate-column\|ingest-token\|rows/ingest)` | always Python |
| `~ ^/api/workbooks(/\|$)`, `~ ^/api/v2/workbooks/{id}/views`, `^~ /api/templates`, `^~ /api/functions`, `^~ /api/entities`, `^~ /api/analytics`, `= /api/me/context`, `= /api/flags`, `= /api/system/stats`, `^~ /api/files/` | Go for the canary percentage, else Python |

The WebSocket `/api/workbooks/{id}/ws` is a retained route: it reaches Python at every percentage, with
the upgrade headers, 300 s timeouts and buffering off copied from `location /api/`. The full list of
retained routes is `apps/backend-go/testdata/contract/retained_workbook_routes.txt`; Go answers 404 for
them (`TestRetainedWorkbookRoutesNotRegistered`), so a mis-route is visible.

## Prerequisites (do not skip)

1. Plans 01, 02 and 03 are cut over (`sqlite-to-postgres.md`, `api-cutover.md`, `settings-cutover.md`);
   `api-go`, `go-split.conf` and `go-routes.conf` are deployed and the `go` line is commented out (0%).
2. `PG_LEAD_STORE=true` on both stacks. With it false Go registers **no** workbook route (it logs why), so
   the canary cannot be enabled by mistake; per-workspace SQLite `leads.db` stays Python-only.
3. `AUTOMATIONS_ENABLED` has the same value on `api`, the Python worker and `api-go`. Go enqueues
   `trigger_eval` jobs for rows it adds only when it is true; a mismatch silently drops or doubles triggers.
4. `IMPORT_MEMORY_BUDGET_MB` (default 512, range 64..65536) sized for concurrent CSV imports. Over budget,
   Go answers 503 `Too many imports in progress` (Retry-After: 5) instead of exhausting memory.
5. `FILES_API_MODE` is unset or `hardened` (default): `/api/files/*` and `/api/system/stats` require
   authentication plus global admin, use a safe path check and never recurse on delete. `legacy` restores
   exact FastAPI behaviour, including the unauthenticated recursive delete: use it only as a temporary
   escape hatch for a client that depends on it, and never on an internet-facing deployment.
6. `DATA_DIR` and the data mount: `api-go` must see the same data directory as `api`. Set `DATA_DIR=/app/data`
   and mount `./data:/app/data` (the same bind mount `api` uses). Go defaults `DATA_DIR` to `data`
   relative to its working directory (`/data` in the distroless image) and `docker-compose.yml` ships no
   `api-go` service, so without this `/api/files/*` and `/api/system/stats` would read an empty or missing
   directory and disagree with Python (which serves `/app/data`).
7. Do not set the provider certification/maturity env on either stack before cutover: Go serves
   `GET /api/meta/providers` with static certification/maturity values, Python computes them from env, and
   they are identical only while no certification env is set (`provider-stats` and `accuracy` stay on
   Python, so `meta/providers` is the only exposure).
8. With `BILLING_ENABLED=true`, keep the Go canary at 0% until plan 06 Task 31 lands. The canary bucket is
   per client, not per workspace, so a billing deployment would see row-edit recompute answer
   `unavailable` for the whole canary.
9. Request-shape and export caps (Go hardening beyond Python, all env-configurable; see the README
   deviations): `WORKBOOK_MAX_ADD_ROWS` (10000), `WORKBOOK_MAX_ROW_KEYS` (2000),
   `WORKBOOK_MAX_VIEW_FILTERS` (50), `WORKBOOK_MAX_VIEW_SORTS` (10), `WORKBOOK_MAX_COLUMNS` (500),
   `EXPORT_MAX_CONCURRENT` (8), `EXPORT_MAX_PER_WORKSPACE` (2), `EXPORT_WRITE_TIMEOUT_SECONDS` (15). Raise
   them only if a client legitimately exceeds them (422 or 429 `Too many exports in progress`).
10. The Go build deployed is the plan-04 build; `make vet test test-integration` and
   `scripts/nginx-routing-check.sh` pass on the release commit.

## Canary ramp

Same knob as plans 02/03: the single `go` line in `go-split.conf` (the shipped state is commented out =
0%). Ramp 5% -> 25% -> 100%, reload after every edit:

```sh
docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload
```

Percentages are global to the split (one bucket per client), not per prefix. Within one stage, verify in
this order before raising the percentage, using the `X-Upstream: go|py` response header to see which side
served a request:

1. read-only routes: `meta`, `analytics`, entities reads, functions reads, templates gallery;
2. workbook reads: list, detail, rows, export, runs, activity, cost, cell trace;
3. writes: create, update, column edits, row edits, import, views, functions apply, entity merge/split.

Dashboards to compare Python vs Go at each stage: 5xx rate and p95 latency per prefix, 4xx rate (a jump in
409/422 means a parity bug), `X-Upstream` split, and Postgres `jobs` enqueue rate for `trigger_eval`.

**Rollback:** comment out the `go` line, reload nginx. There is no data rollback: Go and Python write the
same tables and rows written by Go are readable by Python (and the reverse).

## Verification commands

```sh
cd apps/backend-go && make vet test test-integration
go test ./internal/api -run 'RetainedWorkbook|EveryMigratedWorkbook|FixturesCoverEveryGuard'
scripts/nginx-routing-check.sh       # retained -> py at 0% and 100%; ported -> bucket; /ws upgrade headers
curl -s -D- -o /dev/null -H "Authorization: Bearer $TOKEN" https://HOST/api/workbooks/ | grep -i x-upstream
curl -s -D- -o /dev/null -X POST -H "Authorization: Bearer $TOKEN" https://HOST/api/workbooks/1/run | grep -i x-upstream  # must be absent (py)
```

Cross-stack check at each stage: create a workbook through Go, run/refresh it on Python, then read it
through Go (columns, enrichment cells and `updated_at` order in the list must agree), and the reverse.

## Documented gaps and deliberate deviations

- `BILLING_ENABLED=true`: row recompute (`PATCH rows` with `recompute`) answers
  `{"status":"unavailable", ...}` from Go; deployments with billing enabled keep the Go canary at 0% until
  plan 06 Task 31 lands (prerequisite 8).
- In-flight v2 row cursors issued by Python mid-pagination are decoded by Go; cursors that cannot be
  decoded restart the listing (400 with the exact FastAPI string), so a client paging during the flip may
  need to restart from page 1.
- Files/system routes are hardened by default (decision D1); `legacy` mode is parity.
- `workbook_functions` is global (no RLS); `functions/{id}/apply` keeps FastAPI's lack of cycle validation.
- Negative `limit` on entity routes and other crash-bug hardenings answer 4xx instead of FastAPI's 500.
- Everything else in `apps/backend-go/testdata/contract/README.md` ("Deliberate deviations" and the
  per-task sections) is authoritative.
