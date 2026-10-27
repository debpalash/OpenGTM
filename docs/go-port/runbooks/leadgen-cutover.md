# Runbook: leadgen collection cutover to Go (plan 14)

Go serves 19 routes next to FastAPI: `POST /api/collect`, the 8 `/api/jobs*` routes, `GET /api/system-stats`, `POST /api/leads/{score,verify-email,tech-stack,domain-intel}` and the 5
`/api/settings/sources*` routes. **Three routes stay on FastAPI** (`apps/backend-go/testdata/contract/retained_leadgen_routes.txt`, D4): `POST /api/lead/{id}/enrich`,
`GET /api/v2/search/unified`, `POST /api/v2/scraper/scrape` (plus plan 06's `GET /api/workbooks/{id}/sources/{col}/preview`). **The scrapers never move**: `collect`, `bulk_enrich` and
`source_workbook` stay `executor='python'` (Go only enqueues); `source_health_check` can move to Go behind a flag, and Go then reaches the ddgs probes through the Python worker
(`POST /internal/leadgen/source-probe`). Deviations: `apps/backend-go/testdata/contract/README.md`, section "Plan 14 deviations".

The step that moves data is the **collection ledger cutover** (`collection_jobs`, `collection_job_stages`): a coordinated, **quiesced** step, not a canary. The route canary comes after it.

## Prerequisites

1. Plans 01-06 are cut over (auth and workspaces, settings and secrets, workbooks, leads and signals, enrichment jobs with `PY_WORKER_URL` / `PY_WORKER_TOKEN`). The Alembic revision
   `c9d2e4f6a8b1` (collection ledger tables) or later is applied, and **every Python replica (API, worker, scheduler) runs a release that contains it** (the ledger is chosen by
   `SQLITE_STORES_BACKEND` **and** the `__collection_ledger_backend` marker: deploying the release changes nothing by itself, the ledger stays on SQLite until step 2 writes the marker
   and the processes restart; an older replica would keep writing SQLite after that, so the whole fleet must run the release first). Go's `RequiredGoSchemaRevision` matches the head; `api-go` refuses to start otherwise.
2. `PG_LEAD_STORE=true` on `api-go` (Go supports the Postgres lead store only) and Redis reachable from API and worker (`REDIS_URL`; progress events and `/jobs/{id}/events`; without Redis
   events fall back to the in-process deque, so Go and Python replicas would each show only their own).
3. `PY_WORKER_URL` and `PY_WORKER_TOKEN` reachable from `api-go` (`GET /api/system-stats`, `POST /api/leads/domain-intel` does not need it) and from the Go worker (source health probes).
   Without them system-stats answers 200 with zeros and `X-Stats-Source: degraded` (D5).
4. The stores cutover marker is set (`uv run python -m apps.api.cli mark-cutover`, see `sqlite-to-postgres.md`). It makes every Python process with `SQLITE_STORES_BACKEND=sqlite` refuse to
   start (the same guard also refuses on the ledger marker), so no replica can run collections against a SQLite ledger after the cutover. Every Python process must run with
   `SQLITE_STORES_BACKEND=postgres` already; the ledger marker below is the real switch for **both** Python (`ledger_for`) and Go (`LoadCollectionLedgerBackend`).
5. `TZ` of API, worker and scheduler containers is identical (`UTC` recommended; the ledger stores isoformat text and `source_health.updated_at` uses the session time zone).
6. nginx contains `# ===== BEGIN plan 14: leadgen collection` in `go-routes.conf` and `scripts/nginx-routing-check.sh` passes (0%, 50%, 100%). Both knobs in `go-split.conf` (`$go_bucket`,
   `$go_ws_bucket`) carry the **same** percentage.

## Steps (in order)

### 1. Quiesce

No new collection and no running one from the start of step 2 **until every process has restarted in step 3**. Python reads the marker once per process (long-lived API and scheduler processes keep it until restart; each worker job runs in a fresh process and re-reads it), so a process started before the
marker keeps writing the SQLite ledger; the window must therefore stay closed until the restart is finished.

1. Stop new collects for the window. Add a temporary include **before** `go-routes.conf` in the `server` block (not committed), then `nginx -t && nginx -s reload`:
   ```
   location ~ ^/api/(collect|jobs/[^/]+/retry)$ { if ($request_method = POST) { return 503; } proxy_pass http://api_backend; }
   location ~ ^/api/workbooks/[^/]+/sources(/[^/]+/run)?$ { if ($request_method = POST) { return 503; } proxy_pass http://api_backend; }
   ```
   (the second line stops `source_workbook` jobs, which run a collection inline and write the same ledger). `jobs/{id}/cancel` and `delete` may stay open.
   The copilot can also start collections (`start_collection`, `create_source_workbook`, both approval-gated tools). Close the chat for the window too, in the same include:
   ```
   location = /api/copilot/chat { if ($request_method = POST) { return 503; } proxy_pass http://api_backend; }
   ```
   (Approvals already pending at the start of the window are resumed through the same POST, so they are blocked as well; users re-approve after the window.)
2. Wait until nothing is queued or running. Run as the table owner (RLS applies to the app role); the result must be **empty**:
   ```sql
   SELECT type, status, count(*) FROM jobs
   WHERE type IN ('collect', 'bulk_enrich', 'source_workbook') AND status IN ('pending', 'processing')
   GROUP BY 1, 2;
   ```
   Collect jobs time out after 600 s (`DEFAULT_JOB_TIMEOUT`), `source_workbook` after 1800 s. `migrate_collection_ledger.py cutover` re-checks `collect`/`bulk_enrich` itself and refuses unless
   `--force`; do not use `--force` while rows are active.
3. Stop the Python scheduler's collection producers if you run any (`apps.api.cli collect` and `eval_pipeline` still write the SQLite ledger, see "Known leftovers").

### 2. Backfill, verify, set the marker

From `legacy/python` (database URL in `DATABASE_URL`, data dir = the workspace SQLite files):

```sh
uv run python scripts/migrate_collection_ledger.py backfill --data-dir data --dry-run     # counts only
COLLECTION_LEDGER_CONFIRM=yes uv run python scripts/migrate_collection_ledger.py cutover --data-dir data
```

`cutover` = quiesce check, backfill (idempotent: jobs `ON CONFLICT (id) DO NOTHING`, stages deduplicated by `(job_id, stage, started_at)`), `verify` (job ids, stage counts and **column
content** per workspace) and, only when verify reports **zero mismatches**, the marker `app_settings('__collection_ledger_backend') = 'postgres'` written **last**. If `verify` reports
anything, there is no marker: fix and re-run (it is safe to repeat). `uv run python scripts/migrate_collection_ledger.py verify --data-dir data` re-checks at any time, but only
**before** the first post-cutover write (after it Postgres legitimately differs from the frozen SQLite files).

Check the marker: `SELECT value FROM app_settings WHERE key = '__collection_ledger_backend';` returns `postgres`. Nothing has switched yet: running processes keep their cached choice.

### 3. Restart API, workers and scheduler (this is the flip)

Restart **all** Python processes (API, worker, scheduler; `SQLITE_STORES_BACKEND=postgres` unchanged) and then `api-go`. Python reads the marker once at first use and picks the Postgres
ledger; `api-go` reads it once at startup (`LoadCollectionLedgerBackend`, fail closed: a missing or unreadable marker leaves the leadgen routes unregistered, with a startup error log
line). A Python process that cannot read the marker raises instead of guessing. Only now remove the temporary 503 include and reload nginx. Check the `api-go` startup log has **no**
`leadgen routes not registered` line, and that no `collection ledger marker ... is not 'postgres'` warning appears in the Python logs.

### 4. Route canary

Set `GO_COLLECTION_ROUTES_ENABLED=true` on `api-go` (default false), keep nginx at 0%, restart. Then raise `go-split.conf` for **both** knobs: 0% -> 5% -> 25% -> 100%, watching between steps
(`docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload`). `POST /api/collect` and every `/api/jobs*` route are sticky per workspace (`X-Workspace-Id`); system-stats,
lead utilities and source settings follow the client bucket. Responses carry `X-Upstream: go|py`. With a flag, `PG_LEAD_STORE` or the marker missing Go answers **404** (it does not fall
through), so never raise the canary while the startup log shows `leadgen routes not registered`.

Smoke after each step: `POST /api/collect` (editor) returns `ok:true` with a `queue_job_id`, the job appears in `GET /api/jobs`, a Python worker claims it
(`status` moves `pending -> running -> done`), `POST /api/jobs/{id}/cancel` on a running job makes the worker stop at its next cancel check.

### 5. Source health executor (later)

After **one clean Python-run health cycle** (`GET /api/settings/sources/health` shows a fresh `updated_at` and sane states), enable the Go executor: `GO_EXECUTOR_ENABLED=true` **and**
`GO_EXECUTOR_SOURCE_HEALTH=true` on `api-go`, the Go worker and the Python replicas (the job is global: there is **no workspace cohort**; either flag off hands the job back to Python
without spending a retry). The Go worker needs `PY_WORKER_URL`/`PY_WORKER_TOKEN` (probes run in Python); without them jobs are handed back with one startup warning. Notes:

- `SOURCE_HEALTH_ENABLED` (probe job + bootstrap) and `SOURCE_HEALTH_ENFORCE` (disable/exclude sources) keep their Python meaning and defaults (observe-only). Go reads them from the same env vars.
- Go's `BootstrapSourceHealth` is **not wired to any Go scheduler**: the Python scheduler stays the only scheduler and re-seeds the chain every 60 s. Do not stop the Python scheduler.
- A failed or null probe batch from the worker is an **outage run** (no zero-yield transitions), stricter than Python (D6), so a Python worker outage can never auto-disable sources.
- Go omits Python's 1 s sleep between probe batches (each batch is one RPC; pacing is the worker's). Confirm the worker's `source-probe` endpoint paces batches before raising the load.

## Rollback

| what | how | data moves? |
|---|---|---|
| routes (any percentage) | set both `go-split.conf` knobs back to 0% (comment the `go` lines out), reload nginx. Safe at any time: both stacks read the same `collection_jobs` | no |
| routes, hard | `GO_COLLECTION_ROUTES_ENABLED=false` on `api-go`, restart (Go then registers nothing) | no |
| source health executor | `GO_EXECUTOR_SOURCE_HEALTH=false` (workers hand claimed jobs back to Python) | no |
| **ledger** | Quiesce again (step 1: the 503 include, empty queue). `pg_dump -t collection_jobs -t collection_job_stages` first if you want to keep post-cutover rows. Then `DELETE FROM app_settings WHERE key = '__collection_ledger_backend';` and restart API, workers, scheduler and `api-go` (Python then picks the SQLite ledger again; `api-go` leaves the leadgen routes unregistered, so nginx must be at 0% / `GO_COLLECTION_ROUTES_ENABLED=false` first). `SQLITE_STORES_BACKEND` and `__stores_backend` stay untouched. **There is no reverse-copy tool** (`migrate_collection_ledger.py` has `backfill`, `verify` and `cutover` only): jobs and stages written to Postgres after the cutover are not copied back and disappear from the UI, the SQLite files still hold the pre-cutover history. To cut over again later, run step 2 again (the backfill is idempotent; Postgres-only rows make `verify` report mismatches, so clear or export them first) | yes (loses post-cutover rows unless exported) |

## What to watch

| signal | query / where | investigate when |
|---|---|---|
| collect 5xx | gateway/access log for `POST /api/collect` (503 `Collection queue unavailable` is Go's enqueue failure: nothing was written; 500 is a DB error) | any sustained 5xx |
| ledger rows stuck `running` | `SELECT workspace_id, id, started_at FROM collection_jobs WHERE status = 'running' AND started_at <> '' AND started_at::timestamptz < now() - interval '20 minutes';` (collect timeout is 600 s, retries re-queue) | rows older than the queue timeout plus one retry |
| queue rows without a ledger row | `SELECT j.id, j.workspace_id, j.payload->>'job_id' AS job FROM jobs j LEFT JOIN collection_jobs c ON c.id = j.payload->>'job_id' WHERE j.type = 'collect' AND j.status IN ('pending','processing') AND c.id IS NULL;` | any row (the worker fails with `not found in workspace ledger`; a Python replica that was not restarted after the marker (step 3) is the usual cause) |
| ledger rows without a queue row | `SELECT c.workspace_id, c.id, c.status, c.created_at FROM collection_jobs c WHERE c.status IN ('pending','running') AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type = 'collect' AND j.fire_key = 'collect:' \|\| c.workspace_id \|\| ':' \|\| c.id AND j.status IN ('pending','processing'));` | any row older than a few minutes (a crash between ledger insert and enqueue cannot happen from Go: both are one transaction; Python's two-step enqueue can leave one) |
| degraded stats | `X-Stats-Source: degraded` on `GET /api/system-stats` | persistently (Python worker unreachable) |
| source health | `GET /api/settings/sources/health`: `degraded`/`auto_disabled` counts, `updated_at` age; the worker log `source_health_check` | many sources flip at once (outage run guard should prevent it) |
| `api-go` startup log | `leadgen routes not registered: ...` (flag, `PG_LEAD_STORE`, marker) | after any restart while the canary is above 0% |

(All queries run as the table owner; the app role is RLS-bound to one workspace.)

## Operational notes

- **Restart `api-go` after the ledger cutover** and after any change to `GO_COLLECTION_ROUTES_ENABLED`, `PG_LEAD_STORE` or the marker; config and marker are read at boot.
- Rate limits: `POST /api/collect` 20/min per workspace, `verify-email` 60/min, `tech-stack` 30/min, `domain-intel` **30/min (Go only, Python has none)**; they are per `api-go` replica
  (in-memory `ratelimit`), like slowapi per Python replica.
- Limits that answer 4xx where Python crashed or accepted anything: `collect` `query` over `COLLECT_QUERY_MAX_CHARS` (default 1000) is 422; bodies over 64 KiB (`collect`, `verify-email`,
  `tech-stack`, `domain-intel`) and 1 MiB (`score`) are 413; `events?limit<=0` is 422; `tech-stack` answers 504 when the fetch times out (30 s) and 503 when 16 fetches are already running.
  `tripped_domains` in `system-stats` is hidden from everyone but global admins (it is shared infrastructure state; Python leaks it to every member).
- `system-stats` numbers (`rate_limiter`, `proxy`) are per Python replica: the RPC reaches whichever replica answers (D5), so repeated calls can differ.
- Key order of job JSON is the Python one (`id, query, intent, intent_details, status, tier, attempts, max_attempts, leads_found, proxy_used, error, created_at, started_at, completed_at,
  workspace_id`; detail adds `stages` last). A job whose SQLite file predates Python's `_migrate` had `workspace_id` elsewhere; after the backfill the Postgres column order wins.

## Known leftovers (not changed by this plan)

- `apps/api/analytics.py` per-file job counts and trends, `apps/api/cli.py jobs` / `collect` and `eval_pipeline.py` still use the local SQLite ledger by design: after the flip they show/write a
  different store than the API (operator tools; the trends were already empty). The ledger is not registered in `apps/api/scripts/sqlite_stores/specs.py` either (follow-up).
- `bulk_enrich` and `collect` handlers stay Python indefinitely (D7).
- Run `govulncheck` (built with go1.27) before the cutover; `gosec` is not available here.
- Post-port fix wave: `GET /api/settings/sources` and `/sources/health` need global admin or membership of some workspace (403 `Not authorized`; `/registry` stays open); `health/reset` of an unknown id is 404 `Unknown source`; tech-stack errors are fixed codes (`blocked_url`, `timeout`, `fetch_failed`, `No technologies detected`, `No website URL available`, `robots_disallowed`); stored NaN/Infinity renders `null`; a non-object crt.sh row is skipped; invalid Redis progress entries are skipped without fallback; `verify-email` rejects control/Unicode whitespace and more than one `@`; `/collect` with a NUL byte is 422.
