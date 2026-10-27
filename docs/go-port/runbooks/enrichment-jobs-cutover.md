# Runbook: enrichment jobs cutover to Go (plan 06)

Go serves the workbook run family (estimate, run, stop, cell run, sources, refresh, refresh-policy, migrate,
provider-stats), the workbook WebSocket and the two ingest routes next to FastAPI, and a Go worker can execute
`run_workbook`, `refresh_workbook` and `signal_scan` jobs. Python keeps the scrapers (`source_workbook`, `collect`,
`bulk_enrich`) and every provider that is not HTTP-only (`internal/providers/python_providers.txt`, called through
`POST /internal/provider-call`). 14 route patterns are registered in Go by this plan (`enrichPatterns`, including `GET /api/signals/sources`);
`TestEveryEnrichmentRouteIsPortedOrRetained` fails when a route of `routers/{workbooks,ingest,meta,signals}.py` is in
neither a Go router nor a retained list; 3 stay on Python
(`testdata/contract/retained_enrichment_routes.txt`): `POST /api/v2/workbooks/{id}/generate-column` (LLM, D2),
`GET /api/workbooks/meta/provider-accuracy` (offline eval, D3) and `GET /api/workbooks/{id}/sources/{col}/preview` (D6).

## Routing summary

The plan 06 locations are one delimited section (`BEGIN/END plan 06`) in `infra/nginx/go-routes.conf`, above plan 04's
`~ ^/api/workbooks(/|$)`. Regex locations are first-match, so the order matters; `scripts/nginx-routing-check.sh` asserts it.

| Path | Upstream |
| --- | --- |
| `meta/provider-accuracy`, `sources/{col}/preview`, `generate-column` | always Python, no `X-Upstream` header |
| trailing-slash variants of every ported route (`/run/`, `/ws/`, `/ingest-token/` ...) | always Python (FastAPI redirects, Go would 404) |
| `POST /api/workbooks/{id}/run`, `POST .../rows/{row}/cells/{col}/run` | Go for the canary percentage **keyed per workspace** (`$go_ws_bucket`, header `X-Workspace-Id`; client key when absent) |
| `GET .../ws` | Go for the client bucket; `Upgrade`/`Connection` headers, HTTP/1.1, `proxy_read_timeout 1h` |
| `run/estimate`, `stop`, `meta/provider-stats`, `sources`, `sources/{col}/run`, `refresh`, `refresh-policy`, `migrate`, `ingest-token`, `rows/ingest` | Go for the client bucket |

`go-split.conf` now has TWO percentage lines (`$go_bucket` and `$go_ws_bucket`): keep them equal when ramping. The
per-workspace key makes one workspace's runs hit one stack, so the debit and the enqueue (one Go transaction, D4)
are never split with Python's debit-then-enqueue for the same workspace. The client must send `X-Workspace-Id`
(the web app does); without it the run follows the client key and a workspace may be served by both stacks (safe,
just not sticky).

## Prerequisites (do not skip)

1. Plans 01-05 and 07 are cut over and verified; `api-go`, `go-split.conf` (with the plan 06 `$go_ws_bucket` lines) and
   `go-routes.conf` are deployed with every `go` line commented out (0%).
2. `PG_LEAD_STORE=true` on `api`, `api-go`, the Python worker and the Go worker. Without it Go registers nothing here.
3. The Python Alembic revision is applied on **all** Python workers and the API before `GO_EXECUTOR_ENABLED` is set
   anywhere: Go and Python must agree on `RequiredGoSchemaRevision` / `REQUIRED_GO_SCHEMA_REVISION` (`f2a6b0c4d8e3`, which
   includes the workspace-scoped cache tables, D5). `api-go`, the Go worker and `mcp` refuse to start on a mismatch
   (`queue.CheckSchema`).
4. Provider credentials: plan 03 secrets are readable by Go (`SECRET_KEY`/envelope as on `api-go`), **or** `PY_WORKER_URL`
   points at the Python API (`http://api:8000`) and is reachable from the Go worker and `api-go`; `PY_WORKER_TOKEN`
   (default `INTERNAL_SERVICE_TOKEN`, at least 16 characters, never a `change-me` placeholder) must equal the Python
   side's `INTERNAL_SERVICE_TOKEN`. Without `PY_WORKER_URL` jobs that contain a Python-only provider or an output column
   are handed back to Python (`HandBack`), never failed.
5. Redis (`REDIS_URL`) reachable from `api-go` and the Go worker (run progress events, WebSocket relay). Without it the
   WebSocket answers in echo/pong mode and the UI loses live updates.
6. Manifest directory: the declarative provider manifests are read from `OPENGTM_CONNECTOR_MANIFESTS_DIR` (default
   `legacy/python/apps/api/services/leadgen/enrichment/declarative/manifests`, **relative to the process working
   directory**) with trust store `OPENGTM_CONNECTOR_TRUST_STORE` (default `docs/connectors/trusted-publishers.json`) and
   `CONNECTOR_SIGNATURE_POLICY`. In a container the working directory is not the repo: set both to absolute mounted paths
   on `api-go` and the Go worker. If the index cannot load, `api-go` logs `run: manifest index unavailable` and every run
   stays on Python (safe, but the cutover does nothing): grep for that line after deploy.
7. `EGRESS_ALLOW_HOSTS` (comma separated, operator-only): the SSRF guard (`internal/egress`) blocks loopback, private,
   link-local, CGNAT and IPv4-mapped targets for every provider, `http` column and SMTP probe. List only the internal
   hosts you really need (for example a self-hosted Reacher or a mock vendor). Provider traffic honours it in every
   `APP_ENV`; the `http` column and SMTP probes ignore it outside test/bench.
8. `REACHER_URL` (and `REACHER_ENABLED`, `REACHER_TIMEOUT`) are **global operator** settings read from the process
   environment, never from workspace settings; set them on the Go worker if email verification should use Reacher. They are
   not per-tenant.
9. `CORS_ORIGINS` set in production on `api-go`: the WebSocket handshake must come from the request `Host` or a
   `CORS_ORIGINS` host (Python checked nothing). `*` or empty keeps the Python behaviour and disables the check: do not
   leave it that way on an internet-facing deployment. `WS_MAX_MESSAGE_BYTES` (default 65536) closes oversized frames with 1009.
10. `INGEST_API_ENABLED=true` on both stacks if the ingest routes are used (default off: Go answers 404 like Python).
11. `BILLING_ENABLED` has the same value on `api`, `api-go` and both workers. With billing on, the Go `POST /run` and the
    row-edit recompute (plan 06 Task 31) debit once per run id (`run:<run_id>` per workspace) in the same transaction as
    the enqueue; the worker never debits. The earlier plan 04 caveat that recompute stays `unavailable` under billing is
    **gone**: set `GO_RUN_ROUTES_ENABLED=true` (with `PG_LEAD_STORE`) and the recompute plans, debits and enqueues exactly
    like `/run`. Without `GO_RUN_ROUTES_ENABLED` it still answers `unavailable` under billing, so keep the canary at 0%
    in that configuration.
12. The Go build deployed is the plan 06 build; `make vet test test-integration` and `scripts/nginx-routing-check.sh` pass on
    the release commit.

## Flag order (each step verified before the next)

1. `GO_RUN_ROUTES_ENABLED=true` on `api-go` (the routes register; Go enqueues for Python while no cohort is set).
   Verify with `X-Upstream` at 0% by calling Go directly: `curl http://api-go:8000/api/workbooks/<id>/run/estimate`.
2. Canary the routes: ramp the TWO `go` lines in `go-split.conf` 5% -> 25% -> 100%, reload after each edit
   (`docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload`). Executor still Python: Go runs
   `POST /run` (plan, debit, enqueue with `executor='python'`).
3. Executor cohort: start `worker-go` (`docker compose --profile go up -d worker-go`) with `PY_WORKER_URL`,
   `PY_WORKER_TOKEN`, `REDIS_URL`, manifests dir; then on **api, Python worker and Go worker** set
   `GO_EXECUTOR_ENABLED=true` and `GO_EXECUTOR_WORKSPACES=<ws1,ws2>` (the cohort; an empty list means nobody). Only
   those workspaces' `enrichment`/`waterfall` columns run on Go; everything else is routed to Python by the API.
4. `GO_EXECUTOR_COLUMN_TYPES` (comma separated: `formula`, `http`, `ai_formula`, `research`, `agent`; unknown values are
   dropped) on api and Go worker: widens the cohort to those column types. **Note:** a non-empty value also lifts the
   hand-back for column references and structured targets, so enable it only after steps 1-3 are stable.
   `GO_RUN_NATIVE_PROVIDERS=true` (api only, Go-only flag, default off = Python parity): bulk `/run` normally hands back
   any chain with a provider outside the declarative manifest index (`non_declarative_provider`), exactly like Python,
   so Go-native and Python-worker providers only run on single-cell run. With the flag on, such chains stay on Go
   (native in process, else `PY_WORKER_URL`; an unknown provider is still handed back at run time). Enable it only
   after the Go worker has `PY_WORKER_URL` and global settings (`REACHER_URL`, `SEC_EDGAR_USER_AGENT`, ...) in its env.
5. `GO_EXECUTOR_SIGNAL_SCAN=true` (cohort-gated like step 3, together with `GO_EXECUTOR_ENABLED`) moves `signal_scan` and
   the refresh chain's next occurrence to Go; `SIGNAL_SCAN_MAX_HOT_LEADS` (50), `_MAX_WARM_LEADS` (30),
   `_MAX_LEADS_PER_WORKSPACE` (20), `SIGNAL_SCAN_GLOBAL_MAX_LEADS` (500, 0 = unlimited) apply to both stacks.

Compare Python vs Go per stage: 5xx and 4xx rate per route (`X-Upstream`), `jobs` failure/retry rate by `type` and
`executor`, spend-attempt `uncertain` count, cell error rate, WebSocket connect/close-code mix (4403 = forbidden),
`credit_ledger_entries` rows per `run:<id>` (must be exactly one).

## Billing cutover check (BILLING_ENABLED=true)

1. Pick a test workspace with a known balance below the projected cost of a paid run: `POST /run` answers 402 with
   `{"detail":{"message","balance_usd","required_usd"}}`, nothing is debited or enqueued.
2. Raise the balance; run once: exactly one ledger row `run:<run_id>@ws:<workspace>` and one `run_workbook` job. Repeat the
   same `run_id` concurrently: one debit, the second caller gets the replay.
3. Row edit with `recompute` on a workbook with paid columns and no credits: HTTP 200 with
   `{"status":"blocked","detail":{...}}`, the edit stands, no job. With credits: one debit, one job.
4. All-BYOK runs, `projected <= 0` and billing off never touch the ledger.

## Rollback (no data rollback needed; Go and Python write the same tables)

1. Routes: comment out BOTH `go` lines in `go-split.conf` (0%), reload nginx. In-flight Go runs finish on their executor.
2. Executor: set `GO_EXECUTOR_WORKSPACES` empty (or `GO_EXECUTOR_ENABLED=false`) on api, Python worker and Go worker; new
   jobs are enqueued `executor='python'`. Drop `GO_EXECUTOR_COLUMN_TYPES` and `GO_EXECUTOR_SIGNAL_SCAN` first if set.
3. Hand-back: stop `worker-go` (SIGTERM, 30 s grace) once its in-flight jobs finish. A job the Go worker refuses
   deterministically (for example a Python-only column with no `PY_WORKER_URL`) is handed back by `queue.HandBack`
   (status `pending`, `executor='python'`, no retry spent). Jobs still `pending` with `executor='go'` are not claimed
   by Python: flip them with an operator `UPDATE jobs SET executor='python' WHERE executor='go' AND status='pending'`
   under the owner role. A dispatched paid call that was cut off stays `uncertain` in the spend ledger and is never
   re-sent automatically.
4. `GO_RUN_ROUTES_ENABLED=false` only after step 1: Go then answers 404 for the whole family.

## Verification commands

```sh
cd apps/backend-go && make vet test test-integration
go test ./internal/api -run 'EveryEnrichment|RetainedEnrichment|EveryJobType|EnrichRoutePatterns|FixturesCoverEveryGuard'
scripts/nginx-routing-check.sh        # retained -> py at 0% and 100%; ported -> bucket; /ws upgrade + 1h timeout
curl -s -D- -o /dev/null -X POST -H "Authorization: Bearer $TOKEN" -H "X-Workspace-Id: $WS" https://HOST/api/workbooks/1/run | grep -i x-upstream
```

## Deliberate deviations (details: `apps/backend-go/testdata/contract/README.md`)

- D2/D3/D6: generate-column, provider-accuracy and source preview stay on Python. D1: the job ledger, `collect`,
  `/api/jobs*` and lead enrich stay on Python.
- D4: debit and enqueue are one transaction (Python commits the debit first and can lose credits on an enqueue failure).
  Concurrent double-submit with different run ids still charges twice (Python parity).
- D5: provider and verdict caches are Postgres tables keyed by workspace under RLS; Python keeps its own SQLite cache for
  Python-run cells, so a cell run on each stack can call the vendor once each.
- D7: the workbook WebSocket uses `github.com/coder/websocket`; Origin check, 64 KiB frame cap, ping/pong
  connection caps and the `SSE_MAX_LIFETIME_SECONDS` socket lifetime are Go hardening; invalid JSON frames are ignored (Python drops the socket) and Redis relays continue.
- D8: the Python boundary is one synchronous `POST /internal/provider-call`; credentials resolve on the Python side.
- Workspace-scoped idempotency key for the platform debit; SSRF-guarded egress on every provider, `http` column and probe.
- ipinfo resolves the lead's domain with the system resolver (a DNS query outside the egress guard, as Python); the
  resolved IP is only sent to ipinfo.io as a path segment.
- Recorded fixture for the billed recompute 402 is still open (shape asserted byte-exact in `internal/runstart` tests).
- Post-port fix wave: `POST /api/workbooks/{id}/refresh` is deduplicated while a manual refresh is pending or processing (fire key `refresh-manual:<workbook_id>`; response still `{"status":"refreshing"}`); `from-jobs` with `workbook_id` always persists the merged `job_ids` union; `functions/apply` answers 422 when the change introduces a circular dependency; bulk/import `too_long` 422 bodies show `<omitted>` instead of echoing input; naive condition timestamps are read as UTC (no `TZ` alignment needed).
