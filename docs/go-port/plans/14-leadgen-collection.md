# Leadgen Collection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port everything around lead collection that is not a scraper: the collection job ledger (`jobs`, `job_stages`) moved from per-workspace SQLite to Postgres, the collection API (`POST /api/collect`, the 8 `/api/jobs*` routes, `GET /api/system-stats`), the lead utilities (`/leads/{score,verify-email,tech-stack,domain-intel}`), lead scoring, the source-health state machine with its 5 `settings/sources*` routes and the `source_health_check` job, and the progress publisher. The scrapers, browser tier, proxy pool, rate limiter, staffspy/jobspy, ddgs probes and the `collect`, `bulk_enrich`, `source_workbook` handlers **stay a Python worker**; Go enqueues them with `executor='python'` and Python reads and writes the same Postgres ledger.

**Architecture:** Today the collection API is split across two databases: lead rows are in Postgres (RLS), but `jobs` and `job_stages` live in per-workspace SQLite files (`leads.db`), which is why plan 05 (D1) and plan 06 (D1) left `/collect` and `/jobs*` on FastAPI. This plan first moves the ledger (Python Alembic tables `collection_jobs`, `collection_job_stages`, a backend switch in Python `LeadDB`, a verified backfill), after which both stacks share one source of truth and enqueue, cancel and retry become single-transaction operations (the Python version cannot be atomic across SQLite + Postgres and compensates after failure). Go then serves the API; Python's `JobRunner` keeps executing, calling a small set of internal RPC endpoints only where state is process-local (`system-stats`: proxy pool and rate limiter live in the Python process) or where the work is scraping (`source-probe` for the health job). Pure logic with no I/O (`score_lead`, `decide_collection`, `_apply_transition`, `domain_intel` analysis over DoH/RDAP HTTP) is ported with Python-generated vectors.

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, go-redis v9; existing `internal/{httpx,identity,tenancy,canonjson,queue,pyworker,egress,ratelimit,leadsread,emailverify,providers,progress,leadevents,config}` (plans 02-06). Python/uv for recorder, seed, vectors, Alembic, backfill, and the Python-side RPC endpoints.

**Spec:** `docs/go-port/inventory.md` section 8 row 13 "Leadgen collection" (sections 2, 3 job types, 5 stores, 6 background processes, 7 integrations, 9 risks); `plans/05-leads-signals.md` (D1 ledger, retained list), `plans/06-enrichment-jobs.md` (`pyworker`, `EnqueueOnce`, `emailverify`, `providers`, `python_providers.txt`), `plans/03-settings-secrets.md` (retained `sources*`), `plans/01-sqlite-to-postgres.md` (backfill framework, `SQLITE_STORES_BACKEND`). Python sources: `apps/api/routers/{leads,settings}.py`, `services/leadgen/{db,job_runner,store,scoring,collection_intent,source_health,source_stats,source_registry,progress,proxy_pool,rate_limiter,config}.py`, `services/leadgen/enrichment/{domain_intel,size_heuristic,providers/tech_stack_provider}.py`, `services/job_registry.py`, `services/queue_service.py`, `services/workspace/manager.py` (`workspace_leads_db_path`).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`, `migrations/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 01-06 merged (Postgres chat/functions/memory, identity, tenancy, `leadsread`, `queue.EnqueueOnce`, `pyworker`, `emailverify`, `providers/firmo/techstack`, `Deps.PyWorker`, `Deps.Redis`). Assumes plan 05 `leadsread.Lead` (JSON shape of `Lead.to_dict()`), `leadsread.LeadsByIDs`; assumes plan 06 `emailverify.Cascade`, `providers.Get("tech_stack")`, `queue.EnqueueOnce`, `pyworker.Client`; assumes plan 13 (scrapers worker, if it defines a different RPC path for probes, Task 5 follows it and only the path constant changes). Plans 07-11 are independent of this one.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL: Alembic in `legacy/python/migrations` owns it (Task 2); Task 6 syncs `schema.sql` and bumps `RequiredGoSchemaRevision`. No new Go modules.
- Tenant tables (`collection_jobs`, `collection_job_stages`, leads) only inside `tenancy.InWorkspace`; `source_health` and `source_stats` are global (no workspace column, like Python: sources are shared infrastructure). Runtime role non-superuser `NOBYPASSRLS`. A job id of another workspace answers 404 `Job not found`, never 403.
- **The ledger keeps SQLite value shapes** so JSON is byte-identical: timestamps are `TEXT` ISO strings with `''` defaults (`datetime.isoformat()` with `+00:00`), `intent_details` is a `TEXT` JSON string, integers are `INTEGER`. Key order of a job row (`SELECT *` on SQLite after `_migrate`): `id, query, intent, intent_details, status, tier, attempts, max_attempts, leads_found, proxy_used, error, created_at, started_at, completed_at, workspace_id`; stage row: `id, job_id, stage, status, input_count, output_count, rejected_count, details, started_at, completed_at`. Verified by Task 1 fixtures (a job whose SQLite file was created before `_migrate` has a different column order; documented, the PG table fixes one order).
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, gates verbatim: `POST /collect` = `require_editor` + rate limit 20/min; `GET /jobs*`, `GET /system-stats` = `current_workspace`; cancel/delete/retry = `require_editor`; `verify-email` 60/min and `tech-stack` 30/min = `current_workspace`; `score` = `current_workspace`; `domain-intel` = `require_editor` (no limiter); `settings/sources*` as Python (`GET` open, `PUT` and `health/reset` global admin). Rate limits through `internal/ratelimit` with the slowapi key function (plan 06).
- Contract fixtures are recorded from FastAPI (`legacy/python/scripts/contract_record.py` + `contract_seed.py`, non-superuser role, `TZ=UTC`, `SQLITE_STORES_BACKEND=postgres` so the recorded FastAPI already reads the new ledger), never hand-written. Algorithms (scoring, intent, transitions, domain analysis) pinned by Python-generated vectors; HTTP behaviour (DoH, RDAP, email cascade) by Python-recorded transcripts or plan 06's.
- **Scrapers stay Python.** `collect`, `bulk_enrich`, `source_workbook` keep `executor='python'` (enqueue by Go with `queue.EnqueueOnce`/`Enqueue`, `executor` argument `"python"`); the claim query in the Python worker is unchanged. `source_health_check` becomes `executor='go'` only behind `GO_EXECUTOR_SOURCE_HEALTH` (default false): Go runs the outage guard, transitions and reschedule, Python runs the ddgs probes through `POST /internal/leadgen/source-probe`. A test fails if a Go handler imports a scraper-class dependency (the same `python_providers.txt` rule as plan 06).
- Security and crash hardening deviates from Python only as a documented deviation (D2-D6): the `collect` body (`query` over 1,000 chars, non-string, null) answers 422 instead of a 500; `status` filter on `/jobs` is an allow-list (unknown value returns `[]` exactly as Python's empty result, no error); `limit` on `/jobs/{id}/events` is clamped to `[0, 500]` (Python slices with a negative value); `domain-intel` and `tech-stack` fetches go through `internal/egress` (SSRF-guarded, no redirects, 1 MiB cap); enqueue + ledger insert are one transaction.
- Redis in tests: `TEST_REDIS_URL` (127.0.0.1:56380). Integration tests skip when unset. External HTTP is tested against recorded transcripts or `cmd/providersim`, never the internet.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (documented in `testdata/contract/retained_leadgen_routes.txt`, Task 22): `POST /api/lead/{id}/enrich` (SSE, OSM/DDG scrapers), `GET /api/v2/search/unified` (source registry of academic APIs, not collection; D4), `POST /api/v2/scraper/scrape` (UniversalScraper, browser tier), plan 06's `/sources/{col}/preview`. The collection *execution* is the Python worker, not a route.

## Review Focus

1. **Ledger cutover without lost or duplicated jobs.** Two backends exist until the flag flips: a collect job claimed by the Python worker on SQLite while the API already writes Postgres loses its cancel and status updates. Cutover quiesces (no `pending`/`processing` `collect` queue rows), backfills, verifies counts, then flips `SQLITE_STORES_BACKEND` for API and worker together; the Python worker refuses to start in `sqlite` mode when the marker row says `postgres` (plan 01 guard, extended to `collection_jobs`). Tasks 3, 4, 22.
2. **Enqueue atomicity and cancel semantics.** `POST /collect` inserts the ledger row and the queue row in one transaction (fire key `collect:<ws>:<job_id>` unique, so a client retry cannot double-run); cancel flips ledger status (only from `running`/`pending`, exact error text) and the queue row (`pending`/`processing` only) in one transaction, and the Python runner observes `cancelled` via `_is_cancelled` at its next checkpoint; retry only from `failed`/`cancelled`, 409 text exact, `attempts` reset. Tasks 10, 14, 16.
3. **Tenant isolation of the ledger.** Python's SQLite variant resolved the file from the slug (an earlier IDOR); the Postgres variant filters `workspace_id` in SQL and by RLS. Body `workspace_id` on `/collect` is ignored. The `jobs/{id}/leads` route lists only leads of the caller's workspace whose `collection_job_id` or `source = 'job:<id>'` match. Tasks 10, 14, 15.
4. **Scoring and intent parity.** Python `round()` is banker's rounding (`round(score + SWING*(r-0.5))`), `lead.city in ICP[...]` is case-sensitive, `has_*` properties treat `"N/A"`, `"nan"`, `""` as absent, `normalize_band` coerces ints and ranges, `decide_collection` is regex-driven with Unicode lowercase and whitespace collapse. A vector corpus of at least 300 cases each plus fuzz comparison of Go against recorded Python outputs. Tasks 8, 9.
5. **Source-health safety.** The systemic-outage guard (zero ratio >= `SOURCE_HEALTH_OUTAGE_RATIO` applies no zero transitions), `manual_override` never auto-disables, enable clears disabled state, fail-open semantics, and the job must not self-reschedule when the master flag is off. A Python RPC failure must not mark any source unhealthy (treated as an outage run). Tasks 12, 20.
6. **Process-local state through RPC.** `system-stats` shows the proxy pool and rate limiter of one Python process; with several Python workers the numbers are per replica. Default: the API returns the sum across the replica the RPC reaches and documents it (D5); an unreachable worker returns the same structure with zeroed pool/limiter counts and real job counts rather than 500. Task 17.

## File Structure

```
internal/leadscore/{score,band,tier,icp}.go, icp.json       # score_lead, normalize_band, get_tier (icp.json dumped from Python config)
internal/collectintent/{decide,infer,teamreq,options}.go    # decide_collection and helpers
internal/collectledger/{store,jobs,stages,reconcile}.go     # collection_jobs / collection_job_stages
internal/leadprogress/{publish,recent}.go                   # progress.emit publisher + recent(): Redis pubsub + list formats
internal/sourcehealth/{transition,store,summary}.go         # _apply_transition, health_summary/map, manual_enable, reset
internal/sourcecatalog/{catalog,enabled}.go, sources.json   # DATA_SOURCES, registry summary, get_source_enabled
internal/domainintel/{clean,dns,rdap,analyze,score}.go      # domain_intel
internal/leadjobs/{health_job,handlers}.go                  # source_health_check Go handler (probe via RPC)
internal/api/{collectroute,jobsread,jobswrite,systemstats,leadutils,domainintelroute,settingssources,leadgenscaffold}.go
internal/api/register.go                                    # (modify once, Task 7)
internal/db/queries/{collection_jobs,source_health,collection_leads}.sql (+ gen/)
internal/db/schema.sql                                      # (regenerated, Task 6)
testdata/contract/{collect,jobs,leadutils,sources}/*.json
testdata/contract/retained_leadgen_routes.txt
../../legacy/python/migrations/versions/<rev>_collection_ledger.py
../../legacy/python/apps/api/services/leadgen/{job_ledger.py,db.py,job_runner.py,source_engine.py}   # backend switch, repointed callers
../../legacy/python/apps/api/routers/{internal_leadgen.py,leads.py}                                  # /internal/leadgen/*; leads.py uses the ledger
../../legacy/python/scripts/{contract_seed.py,contract_cases/leadgen.yaml,gen_leadgen_vectors.py,migrate_collection_ledger.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/leadgen-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time; larger waves run as batches). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors; 2 Python Alembic ledger tables; 3 Python ledger backend switch and repointed callers; 4 Python backfill and verify; 5 Python RPC endpoints and executor routing; 6 Go schema sync and sqlc; 7 Go scaffold | T1 owns `contract_seed.py`, `gen_leadgen_vectors.py`; T2 owns migrations; T3 owns `job_ledger.py`, `db.py`, `job_runner.py`, `source_engine.py`, `routers/leads.py` job paths; T4 owns `migrate_collection_ledger.py`; T5 owns `internal_leadgen.py`, `main.py` include line, `source_health.py` enqueue site; T6 owns `schema.sql`, the revision constant and the three `.sql` files; T7 owns `register.go`, `Deps`, stub files |
| 1 | 8 leadscore, 9 collectintent, 10 collectledger, 11 leadprogress, 12 sourcehealth, 13 sourcecatalog + domainintel | 6 disjoint packages (13 owns two packages and runs last in the batch) |
| 2 | 14 collect route, 15 jobs read routes, 16 jobs write routes, 17 system-stats route, 18 lead utilities, 19 domain-intel route | each owns exactly one stub `internal/api/*.go` and one fixture dir; 14 consumes 9, 10, 11; 15 consumes 10, 11; 16 consumes 10; 18 consumes 8 |
| 3 | 20 source_health_check Go handler, 21 settings sources routes | 20 owns `internal/leadjobs/` and a `cmd/worker/main.go` registration line; 21 owns `settingssources.go` |
| 4 (sequential) | 22 coverage, cross-stack tests, nginx, runbook | consumes everything |

---

### Task 1: Python seed, recorder cases, vectors

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/leadgen.yaml`, `legacy/python/scripts/gen_leadgen_vectors.py`
- Create (output, committed): `testdata/contract/{collect,jobs,leadutils,sources}/*.json`, `internal/{leadscore,collectintent,collectledger,sourcehealth,sourcecatalog,domainintel,leadprogress}/testdata/*.json`, `internal/leadscore/icp.json`, `internal/sourcecatalog/sources.json`

**Interfaces:**
- Consumes: plan 04/05 seed (workspaces A/B, editor/viewer/admin, leads with `collection_job_id` and `source='job:<id>'`).
- Produces: seed additions (workspace A: 7 jobs across `pending/running/done/failed/cancelled` with 2 stages each, one job with `intent_details` of `{}` and one with a clarification payload, leads attached to two jobs by `collection_job_id` and by `source`; workspace B: 2 jobs; `source_health` rows in `healthy/degraded/auto_disabled` with and without `manual_override`; `source_stats` rows; a Redis `leadgen:progress:<ws>:recent` list of 6 events for one job). Recorder cases: collect happy path, clarification response (`ok:false`), empty query 400, viewer 403, rate limit 429, jobs list with `status` and `__all__`, detail parsed `intent_details` and stage `details` (valid and invalid JSON), stages 404, leads of a job, events with `limit`, cancel (running, done, foreign), delete with and without `keep_leads`, retry (failed ok, done 409, foreign 404), system-stats, score (by `lead_id`, inline, neither 400, missing 404), verify-email (valid, no `@` 400), tech-stack (disabled 409, empty 400), domain-intel (400 variants), settings sources GET/health/PUT/reset/registry for admin and non-admin. Vector files: 300 `score_lead` cases (every weight, city tiers, `"N/A"` strings, `normalize_band` formats, reliability nudge with `r` in {None, 0, 0.5, 0.49, 1} and `.5` rounding), 300 `decide_collection` cases (domains, company-team, people, explicit intent overrides, Unicode, whitespace, empty), 60 `_apply_transition` sequences (outage flag, manual override, thresholds, ewma), 40 `domain_intel` analyses from recorded DoH/RDAP transcripts, `progress` event JSON (separators, float `ts`), source `enabled` resolution. `icp.json` and `sources.json` are dumps of Python `ICP`, `SCORING_WEIGHTS`, `SCORE_TIERS` and `DATA_SOURCES` + `get_source_summary()`.

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps; ledger rows written through the Task 3 backend so recorded FastAPI reads Postgres).
- [ ] **Step 2: Write the cases and the vector script** (they import the real Python modules; the recorder runs with `SQLITE_STORES_BACKEND=postgres`).
- [ ] **Step 3: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/leadgen.yaml` and `uv run python scripts/gen_leadgen_vectors.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 4: Commit** (Go/doc side only) `test(go): leadgen contract fixtures and vectors (RED)`.

### Task 2: Python Alembic: collection ledger tables

**Files:**
- Create: `legacy/python/migrations/versions/<rev>_collection_ledger.py`
- Test: `legacy/python/tests/test_migration_collection_ledger.py`

**Interfaces:**
- Produces: `collection_jobs(id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, query TEXT NOT NULL, intent TEXT NOT NULL DEFAULT 'market_search', intent_details TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT 'pending', tier INTEGER NOT NULL DEFAULT 1, attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 3, leads_found INTEGER NOT NULL DEFAULT 0, proxy_used TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL DEFAULT '', completed_at TEXT NOT NULL DEFAULT '')` with index `(workspace_id, status)` and `(workspace_id, created_at DESC)`; `collection_job_stages(id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, job_id TEXT NOT NULL REFERENCES collection_jobs(id) ON DELETE CASCADE, stage TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'running', input_count INTEGER NOT NULL DEFAULT 0, output_count INTEGER NOT NULL DEFAULT 0, rejected_count INTEGER NOT NULL DEFAULT 0, details TEXT NOT NULL DEFAULT '{}', started_at TEXT NOT NULL DEFAULT '', completed_at TEXT NOT NULL DEFAULT '')` with index on `job_id`. Both: ENABLE + FORCE RLS on `workspace_id = current_setting('app.workspace_id', true)`, grants to the runtime role (pattern of `b3c4d5e6f7a8`). Job ids are global (uuid4 hex) so the PK is the id alone; the policy still isolates reads. The `llm_usage` ledger already has a Postgres store (`llm_usage_daily`, `PgLeadStore`) and `activity_log` has no readers (Task 1 greps to confirm and the test records the result); neither moves.

- [ ] **Step 1: Write the failing test** (`upgrade` creates both tables, RLS blocks cross-workspace select with a non-superuser role, cascade delete of stages, `downgrade` drops both). **Step 2: Run** `uv run pytest tests/test_migration_collection_ledger.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS and `alembic heads` shows one head.
- [ ] **Step 5: Commit** (Python side unversioned: note the revision id in the PR description).

### Task 3: Python ledger backend switch and repointed callers

**Files:**
- Create: `legacy/python/apps/api/services/leadgen/job_ledger.py`
- Modify: `legacy/python/apps/api/services/leadgen/db.py` (job and stage methods delegate), `legacy/python/apps/api/services/leadgen/job_runner.py` (53 raw `db.conn.execute` sites become ledger calls), `legacy/python/apps/api/services/workbook/source_engine.py` and `routers/leads.py` (`_workspace_job_db` and the three `db.conn.execute` job updates)
- Test: `legacy/python/tests/test_job_ledger_pg.py`, existing leadgen/job tests parametrised over both backends

**Interfaces:**
- Produces: `class JobLedger` with the exact method set and semantics of `LeadDB` job methods (`create_job`, `claim_job`, `complete_job`, `fail_job` incl. requeue under `max_attempts`, `cancel_job` guarded by `status IN ('running','pending')`, `delete_job`, `retry_job` guarded by `status IN ('failed','cancelled')`, `get_jobs(status, limit=50)`, `create_stage`, `complete_stage`, `get_job_stages`, `get_job_detail`, `get_job_leads`) plus `set_workspace(job_id, ws)`; `ledger_for(workspace_id, slug)` returns the SQLite implementation (`workspace_leads_db_path(slug)`) or the Postgres implementation (short transaction, `set_config('app.workspace_id', ...)` first) by `SQLITE_STORES_BACKEND`. Every raw job/stage SQL string in `job_runner.py` moves into a `JobLedger` method; `reconcile_collect_job_failure` and `handle_collect` use the ledger.
- Behaviour that must not change: the SQLite path byte-for-byte (default), time stamps `datetime.now(timezone.utc).isoformat()`, `INSERT OR IGNORE` semantics (`ON CONFLICT (id) DO NOTHING`), the `(provider,date)` upsert of `llm_usage` untouched.

- [ ] **Step 1: Write failing tests**: parametrise the existing job tests over `{sqlite, postgres}`; add `test_pg_create_job_idempotent`, `test_pg_cancel_only_from_running_or_pending`, `test_pg_retry_resets_attempts_and_error`, `test_pg_fail_requeues_until_max_attempts`, `test_pg_delete_job_cascades_stages`, `test_pg_ledger_rls_blocks_other_workspace`, `test_job_runner_has_no_raw_job_sql` (grep guard: no `UPDATE jobs`/`INSERT INTO job_stages` string outside `job_ledger.py`).
- [ ] **Step 2: Run** `uv run pytest tests/test_job_ledger_pg.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** `uv run pytest tests -k "job or collect or leadgen" -q` under both backends -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 4: Python backfill and verify

**Files:**
- Create: `legacy/python/scripts/migrate_collection_ledger.py`; modify `legacy/python/scripts/sqlite_stores/verify.py` (plan 01) only to register the new store
- Test: `legacy/python/tests/test_migrate_collection_ledger.py`

**Interfaces:**
- Produces: `migrate(data_dir, pg_url, *, dry_run=False) -> Report` iterating `_iter_workspaces()` (plan 01), copying `jobs` and `job_stages` from each `leads.db` (column order tolerant: reads by name, missing `workspace_id` filled from the owning workspace, `intent`/`intent_details` defaults), idempotent (`ON CONFLICT (id) DO NOTHING` for jobs; stages deduplicated by `(job_id, stage, started_at)`), orphan stages (job missing) skipped and counted, rows whose `workspace_id` disagrees with the file's workspace trusted to the **file** (counted), `--dry-run` reports counts only; `verify(...)` compares per-workspace counts and the sets of ids; refuses to run while a `collect`/`bulk_enrich` queue row is `pending`/`processing` unless `--force`; writes the marker row `app_settings('__collection_ledger_backend') = 'postgres'` last.

- [ ] **Step 1: Write failing tests**: `test_backfill_copies_jobs_and_stages`, `test_backfill_is_idempotent`, `test_orphan_stage_skipped_and_counted`, `test_foreign_workspace_id_in_row_ignored`, `test_old_column_order_db`, `test_refuses_with_active_collect_jobs`, `test_dry_run_writes_nothing`, `test_verify_detects_missing_job`, `test_marker_written_only_after_verify`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 5: Python internal leadgen RPC endpoints and executor routing

**Files:**
- Create: `legacy/python/apps/api/routers/internal_leadgen.py`; modify `legacy/python/apps/api/main.py` (include line), `legacy/python/apps/api/services/leadgen/source_health.py` (`_enqueue_health_if_absent` and `_reschedule_next` choose `executor` by `GO_EXECUTOR_SOURCE_HEALTH`; the Python handler stays for rollback)
- Test: `legacy/python/tests/test_internal_leadgen.py`

**Interfaces:**
- Auth: same bearer as plan 06 `internal_provider.py` (constant-time compare, 401 without).
- `POST /internal/leadgen/system-stats` `{}` -> `{"proxy_pool": ProxyPool().stats(), "rate_limiter": RateLimiter().stats()}` (the singletons of the serving process).
- `POST /internal/leadgen/source-probe` `{"sources":[names], "timeout_s": float}` -> `{"yields": {"<name>": int|null}}` running `_run_probe` over the batch with the existing concurrency and 1 s inter-batch sleep inside the Python worker; `null` = the probe raised; hard cap `timeout_s` 600 with the task cancelled on client disconnect; sources outside the registry are ignored; no key or proxy URL in the answer.
- `GET /internal/leadgen/source-list` -> `{"sources":[{"name","...probe fields"}]}` = `_all_probe_sources()` (what Go must iterate).

- [ ] **Step 1: Write failing tests**: `test_requires_bearer`, `test_system_stats_shape`, `test_probe_unknown_source_ignored`, `test_probe_exception_is_null`, `test_probe_cancelled_on_disconnect`, `test_probe_never_returns_proxy_urls`, `test_health_enqueue_executor_follows_flag`.
- [ ] **Step 2: Run** `uv run pytest tests/test_internal_leadgen.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 6: Go schema sync and sqlc queries

**Files:**
- Modify: `internal/db/schema.sql` (via `make schema` against a migrated database), `internal/config/config.go` (`RequiredGoSchemaRevision`)
- Create: `internal/db/queries/{collection_jobs,source_health,collection_leads}.sql`
- Test: `internal/db/schema_test.go` (existing drift test)

**Interfaces:** Consumes the Task 2 revision id. Produces named queries: `CJCreate` (`ON CONFLICT (id) DO NOTHING`), `CJSetWorkspace`, `CJList(status filter nullable, limit)`, `CJGet`, `CJStages(job_id)`, `CJCancel` (guarded UPDATE `RETURNING`), `CJRetry` (guarded UPDATE), `CJDelete` (stages cascade), `CJMarkFailedEnqueue`; `SHAll`, `SHGet`, `SHUpsert`, `SHManualEnable`, `SHReset`, `SHCreateIfMissing`; `LeadsByCollectionJob(job_id, limit)` (`collection_job_id = $1 OR source = 'job:' || $1 ORDER BY score DESC, id DESC`; the Python store orders by `score DESC` only, id tiebreak is documented hardening for stable output); `schema.sql` gains `collection_jobs`, `collection_job_stages`, bumped revision (the worker refuses to start below it).

- [ ] **Step 1: Write failing test** `TestSchemaHasCollectionLedger` (tables, PK, RLS flags, cascade). **Step 2: Run** `make test` -> FAIL. **Step 3:** `make schema`, write queries, `make sqlc`, bump revision. **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): collection ledger schema (RED)`, `feat(go): sync collection ledger schema and queries`.

### Task 7: Go scaffold

**Files:**
- Modify: `internal/api/register.go` (+`RegisterCollect`, `RegisterJobs`, `RegisterLeadUtils`, `RegisterSourceSettings` lines), `internal/config/config.go` (`GO_COLLECTION_ROUTES_ENABLED` default false, `GO_EXECUTOR_SOURCE_HEALTH` default false, `COLLECT_QUERY_MAX_CHARS` 1000, `SYSTEM_STATS_RPC_TIMEOUT_SECONDS` 3)
- Create: stub files `internal/api/{collectroute,jobsread,jobswrite,systemstats,leadutils,domainintelroute,settingssources}.go`, `internal/api/leadgenscaffold.go`
- Test: `internal/api/leadgenscaffold_test.go`

**Interfaces:** Produces `Register*(r *httpx.Router, d Deps)` registering nothing and logging when `GO_COLLECTION_ROUTES_ENABLED` or `PG_LEAD_STORE` is false or the ledger backend marker is not `postgres`; the exact route table of Tasks 14-21.

- [ ] **Step 1: Write failing tests**: `TestLeadgenRoutesNotRegisteredWithoutFlags`, `TestLeadgenRoutePatterns` (exactly the 19 routes below: `POST /api/collect`, `GET /api/jobs`, `GET /api/jobs/{job_id}`, `.../stages`, `.../leads`, `.../events`, `POST .../cancel`, `DELETE /api/jobs/{job_id}`, `POST .../retry`, `GET /api/system-stats`, `POST /api/leads/{score,verify-email,tech-stack,domain-intel}`, and the 5 `/api/settings/sources*` routes with `/sources/health` and `/sources/registry` ordered before `/sources/{source_id}`), `TestLedgerMarkerRequired`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): leadgen scaffold (RED)`, `feat(go): leadgen route scaffold`.

### Task 8: Lead scoring

**Files:** Create `internal/leadscore/{score,band,tier,icp}.go`, `internal/leadscore/score_test.go` (embeds `icp.json`)

**Interfaces:** Consumes `leadsread.Lead`. Produces `func Score(l Lead, reliability *float64) int` (= `score_lead`: weights, specialization match with the `else` half-credit branch, multi-tag bonus, city tiers with exact-case membership, decision-maker JSON with 2+ entries, multiple-contacts, founded year/revenue/glassdoor bonuses, cap 100, then `ApplyReliability` with `math.RoundToEven`); `func NormalizeBand(v any) string` (= `size_heuristic.normalize_band`); `func Tier(score int) string` (= `get_tier` over `SCORE_TIERS`); `type Lead` accessor interface so `Lead.from_dict` inline bodies (unknown keys ignored, wrong types coerced as Python dataclass would not: documented, wrong types answer 422 in the route) can be scored; `RELIABILITY_SWING` from env `SOURCE_RELIABILITY_SWING` (default 8).

- [ ] **Step 1: Write failing tests**: `TestScoreVectors` (300 cases), `TestReliabilityNudgeBankersRounding` (x.5 cases), `TestHasPropertiesTreatNAAndNanAsAbsent`, `TestNormalizeBandVectors`, `TestTierBoundaries`, `TestScoreCapAt100`, `TestSpecializationSubstringBothDirections`, `TestDecisionMakersInvalidJSONIgnored`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): lead scoring (RED)`, `feat(go): lead score and band normalisation`.

### Task 9: Collection intent

**Files:** Create `internal/collectintent/{decide,infer,teamreq,options}.go`, tests

**Interfaces:** Produces `func Decide(query string, intent *string) Decision` (= `decide_collection(query, intent)`), `func Infer(query string) string` (= `infer_collection_intent`), `ExtractCompanyTeamRequest`, `ExtractExplicitPeopleRequest`, `NormalizeDomain`; `type Decision` with `ToDict() *Obj` in the Python key order (`query, intent, domain, entity, clarification_kind, clarification_required, reason, options`) and `CanCollect()`; option objects in `IntentOption.to_dict` order. Pure; regexes translated to RE2 (a test asserts none of the Python patterns use lookaround or backreferences; if one does the task stops and records an RPC fallback in the PR, D3). Used by Task 14 and (via Python) by plan 12's pre-route.

- [ ] **Step 1: Write failing tests**: `TestDecideVectors` (300 cases), `TestInferIntentVectors`, `TestCompanyTeamRequestVectors`, `TestExplicitPeopleRequestVectors`, `TestUnicodeLowerAndWhitespaceCollapse`, `TestPythonRegexesAreRE2Compatible`, `TestDecideBoundedOnLongInput` (1 MiB under 100 ms).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): collection intent (RED)`, `feat(go): collection intent decisions`.

### Task 10: Collection ledger store

**Files:** Create `internal/collectledger/{store,jobs,stages,reconcile}.go`, tests (uses Task 6 queries)

**Interfaces:** Produces `type Job struct` marshalling in the Task 1 key order (all timestamps as stored strings; `intent_details` raw string); `func Create(ctx, tx pgx.Tx, ws, id, query, intent, intentDetails string, now time.Time) error`; `List(ctx, tx, status string, limit int) ([]Job, error)` (default 50; `created_at DESC`, id tiebreak documented); `Detail(ctx, tx, id) (*JobDetail, error)` (job + `stages` last, with `intent_details` and stage `details` parsed to objects, invalid or empty JSON -> `{}`, exactly as the route does); `Stages(ctx, tx, id)`; `Cancel(ctx, tx, id, now) (changed bool, err error)` (guarded, `error='Cancelled by user'`, `completed_at=now.isoformat()`), `Retry(ctx, tx, id) (changed bool, err error)` (resets `status, error, completed_at, attempts`), `Delete(ctx, tx, id)`, `MarkEnqueueFailed(ctx, tx, id, msg, now)`; `func CancelQueueJob(ctx, tx, ws, id string, now time.Time) (int, error)` (= `_cancel_collection_queue_job`: `fire_key = 'collect:<ws>:<id>'`, status in `pending`/`processing`, sets `cancelled`, `completed_at`, `error='Cancelled by user'`, clears `worker_id`/`locked_at`); `func Reconcile(ctx, tx, id string, willRetry bool, errMsg string, now time.Time) error` (= `reconcile_collect_job_failure`, kept for a future Go failure handler; tested now).

- [ ] **Step 1: Write failing tests**: `TestJobJSONMatchesFixtures`, `TestDetailParsesIntentAndStageDetails`, `TestInvalidDetailsJSONBecomesEmptyObject`, `TestCancelOnlyFromRunningOrPending`, `TestRetryOnlyFromFailedOrCancelled`, `TestCancelQueueJobOnlyActiveRows`, `TestDeleteCascadesStages`, `TestCreateIdempotent`, `TestListStatusFilterAndLimit`, `TestLedgerTenantIsolation` (pool of 1 alternating tenants), `TestReconcileRetryVsFinal`, `TestTimestampFormatIsPythonIsoformat` (microseconds, `+00:00`).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): collection ledger store (RED)`, `feat(go): collection job and stage store`, `feat(go): collection queue cancel and failure reconcile`.

### Task 11: Progress publisher and history reader

**Files:** Create `internal/leadprogress/{publish,recent}.go`, tests

**Interfaces:** Consumes `Deps.Redis`. Produces `type Bus struct` (`NewBus(rdb *redis.Client)`; nil client = no-op like Python without `REDIS_URL`); `func (b *Bus) Emit(ctx, workspaceID, eventType string, data map[string]any) map[string]any` building `{"type", "ts": time.time(), ...data, "workspace_id"}` (key order as Python; a server-side `workspace_id` wins over the data value) and doing the pipeline `PUBLISH leadgen:progress:<ws>`, `LPUSH ...:recent`, `LTRIM 0 499` with `json.dumps(default=str, separators=(",",":"))` bytes (note: compact separators here, unlike the SSE relay); never publishes without a workspace id; `func (b *Bus) Recent(ctx, workspaceID string, limit int) ([]map[string]any, error)` (limit clamped to `[0,500]`, `LRANGE 0 limit-1` then reversed to oldest-first, invalid JSON entries skipped; Redis error -> empty slice, not an error).

- [ ] **Step 1: Write failing tests**: `TestEmitBytesMatchPythonVectors`, `TestEmitNeverUnscoped`, `TestEmitPublishesAndTrimsTo500`, `TestEmitRedisDownIsNoop`, `TestRecentOrderOldestFirst`, `TestRecentClampsLimit`, `TestRecentSkipsInvalidEntries`, `TestRecentTenantKeyIsolation`, `TestEmittedEventReadableByPlan05SSERelay` (published event arrives through `leadevents` relay re-encoded).
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): lead progress bus (RED)`, `feat(go): lead progress publisher and history`.

### Task 12: Source health state machine and store

**Files:** Create `internal/sourcehealth/{transition,store,summary}.go`, tests

**Interfaces:** Consumes Task 6 queries. Produces `type Row struct` with `ToAPI() *Obj` (key order of `SourceHealth.to_api`, `ewma_yield` rounded to 3 decimals with `math.RoundToEven`, timestamps `isoformat()` naive-UTC); `func ApplyTransition(r *Row, probeYield int, outage bool, now time.Time, th Thresholds)` (= `_apply_transition`; `EWMA_ALPHA` constant dumped from Python; thresholds from env `SOURCE_HEALTH_{DEGRADE,DISABLE,RECOVER}_THRESHOLD` defaults 3, 6, 2); `func Summary(ctx, tx, flags Flags) (*Obj, error)` (`enforce, probe_enabled, total, by_state, sources`), `Map(ctx, tx) (map[string]*Obj, error)`, `ManualEnable(ctx, tx, name)` (get-or-create, pin `manual_override`, clear disabled state and `consecutive_zero`), `Reset(ctx, tx, name)` (nil when no row), `DisabledNames(ctx, tx, enforce bool)` (fail-open: empty when enforcement off); `func ApplyRun(ctx, tx, yields map[string]int, outage bool, now)` persisting one run atomically. Global tables: runs under the dedicated app pool without a workspace GUC (no RLS on these tables; a test asserts the grants).

- [ ] **Step 1: Write failing tests**: `TestTransitionVectors` (60 sequences), `TestOutageAppliesNoZeroTransitions`, `TestNonzeroDuringOutageStillRecovers`, `TestManualOverrideNeverAutoDisabled`, `TestManualEnableClearsDisabledAndPins`, `TestResetBaseline`, `TestSummaryShapeAndByState`, `TestEWMARoundingBankers`, `TestDisabledNamesEmptyWhenEnforceOff`, `TestApplyRunAtomic` (one failing row rolls back all).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): source health (RED)`, `feat(go): source health transitions`, `feat(go): source health store and summary`.

### Task 13: Source catalog and domain intelligence

**Files:** Create `internal/sourcecatalog/{catalog,enabled}.go`, `internal/domainintel/{clean,dns,rdap,analyze,score}.go`, tests (embeds `sources.json`)

**Interfaces:**
- `sourcecatalog`: `func Sources() []Source` (id, name, icon, description, strategy, category, default_enabled, from `DATA_SOURCES`), `func Registry() *Obj` (= `get_source_summary()`), `func Enabled(ctx, settings Getter, id string) bool` (= `get_source_enabled`: `SOURCE_<ID>_ENABLED` via the plan 03 settings getter, `""` -> default, else `== "1"`), `func SetEnabled(ctx, settings Setter, id string, on bool) error`. Drift guard: a test fails if `sources.json` differs from the committed hash recorded by Task 1.
- `domainintel`: `func Clean(raw string) string` (= `clean_domain`), `func Analyze(ctx, c *http.Client, domain string) (*Obj, error)` (DoH to `cloudflare-dns.com`, RDAP, hosting and mail-provider tables, legitimacy score; result key order from vectors); every fetch through `internal/egress` (the DoH/RDAP hosts are fixed; the user domain only appears in query values, a test asserts no request targets a user-supplied host), bounded bodies, 15 s overall deadline, parallel queries bounded to 5.

- [ ] **Step 1: Write failing tests**: `TestSourcesMatchPython`, `TestRegistrySummaryMatchesPython`, `TestEnabledResolution` (empty, `1`, `0`, unknown source defaults false), `TestCatalogDriftGuard`, `TestCleanDomainVectors`, `TestAnalyzeTranscripts` (40 recorded analyses incl. NXDOMAIN, RDAP 404, malformed JSON, timeouts), `TestAnalyzeNeverContactsUserHost`, `TestAnalyzeDeadline`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): source catalog and domain intel (RED)`, `feat(go): source catalog`, `feat(go): domain intelligence analysis`.

### Task 14: POST /api/collect

**Files:** Modify stub `internal/api/collectroute.go`; tests; fixtures `testdata/contract/collect/`

**Interfaces:** Consumes Tasks 8-11. Produces `POST /api/collect` (`require_editor`, 20/min): body `{query, intent?, workspace_id?}` (workspace id ignored); `query.strip()` empty -> 400 `query is required`; `Decide`; clarification -> 200 `{"ok": false, ...decision.to_dict(), "message": reason}` with **no ledger row and no queue row**; otherwise in **one transaction**: `collectledger.Create` (id = uuid4 hex, `intent_details = json.dumps(decision.to_dict())`, `workspace_id` set), `queue.Enqueue`/`EnqueueOnce` of job type `collect` with executor `python` and payload `{job_id, query, intent, workspace_id, slug}` and fire key `collect:<ws>:<job_id>`; commit; then `bus.Emit("job_created", {job_id, query, workspace_id, message: "Collection queued: <query>"})` after commit (Redis failure never fails the request). Response `{"ok": true, "job_id", "queue_job_id", "query", "intent", "workspace_id"}`. An enqueue failure rolls back everything and answers 503 `Collection queue unavailable` (no compensating row needed, unlike Python).

- [ ] **Step 1: Write failing tests**: `TestCollectMatchesFixtures`, `TestCollectEmptyQuery400`, `TestCollectClarificationWritesNothing`, `TestCollectViewer403`, `TestCollectRateLimit429`, `TestCollectBodyWorkspaceIgnored`, `TestCollectLedgerAndQueueAtomic` (inject enqueue error: no ledger row), `TestCollectQueueRowShape` (job type, executor `python`, fire key, payload keys and order equal a Python-enqueued row; a Python worker integration test in Task 22 claims it), `TestCollectQueryTooLong422`, `TestCollectNonStringQuery422`, `TestCollectProgressEventAfterCommit`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): collect route (RED)`, `feat(go): collect route with atomic ledger and queue insert`.

### Task 15: Jobs read routes

**Files:** Modify stub `internal/api/jobsread.go`; tests; fixtures `testdata/contract/jobs/` (read cases)

**Interfaces:** Produces `GET /api/jobs?status=` (`__all__` and empty mean no filter; JSON array of rows), `GET /api/jobs/{job_id}` (detail; 404 `Job not found`), `GET .../stages` (array with `details` parsed; 404), `GET .../leads` (404 first, then leads of the caller's workspace through `LeadsByCollectionJob` limit 10,000, each in `Lead.to_dict()` shape via plan 05 `leadsread`), `GET .../events?limit=100` (404 first; events from `bus.Recent(ws, 500)` filtered to `job_id == id` and a truthy `message`, last `limit`; `{"events": [...]}`).

- [ ] **Step 1: Write failing tests**: `TestJobsReadMatchesFixtures`, `TestJobsStatusFilterAllowList`, `TestJobDetail404ForForeignWorkspace`, `TestJobLeadsBothAttachmentForms` (by `collection_job_id` and `job:<id>` source), `TestJobLeadsCrossWorkspaceEmpty`, `TestJobEventsFilteredByJobAndMessage`, `TestJobEventsLimitClamped`, `TestJobEventsRedisDownEmpty`, `TestJobDetailInvalidDetailsJSON`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): jobs read routes (RED)`, `feat(go): jobs list, detail, stages, leads and events routes`.

### Task 16: Jobs write routes

**Files:** Modify stub `internal/api/jobswrite.go`; tests; fixtures `testdata/contract/jobs/` (write cases)

**Interfaces:** Produces `POST /api/jobs/{job_id}/cancel` (`require_editor`; 404 `Job not found`; one transaction: ledger cancel + `CancelQueueJob`; `{"ok": true, "message": "Job <id> cancelled"}` even when the job was already terminal, as Python), `DELETE /api/jobs/{job_id}?keep_leads=false` (404; cancels the queue half; unless `keep_leads` deletes the job's leads (both attachment forms) in the same transaction through plan 05 `leadswrite` delete so lead events and audit stay consistent; deletes ledger row; `{"ok": true, "message": "Job <id> deleted", "leads_kept": <bool>}`), `POST /api/jobs/{job_id}/retry` (404; non `failed`/`cancelled` -> 409 `Only failed or cancelled jobs can be retried`; in one transaction ledger retry + `EnqueueOnce(collect, executor python, payload {job_id, query, workspace_id, slug}, fire key)`; `{"ok": true, "queue_job_id", "message": "Job <id> queued for retry"}`; an enqueue failure rolls the retry back and answers 503 `Collection queue unavailable`).

- [ ] **Step 1: Write failing tests**: `TestJobsWriteMatchesFixtures`, `TestCancelSetsLedgerAndQueueAtomically`, `TestCancelTerminalJobStillOk`, `TestCancelForeignJob404`, `TestDeleteKeepLeadsFalseRemovesLeads`, `TestDeleteKeepLeadsTrueKeepsLeads`, `TestDeleteForeignJob404NoLeadDeleted`, `TestRetryOnlyFailedOrCancelled409`, `TestRetryEnqueueFailureRollsBack`, `TestRetryTwiceConcurrentSingleQueueRow`, `TestWriteRoutesViewer403`, `TestRunnerSeesCancelledStatus` (a Python-written `_is_cancelled` read after Go cancel; Task 22 cross-stack).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): jobs write routes (RED)`, `feat(go): job cancel and retry`, `feat(go): job delete`.

### Task 17: System stats route

**Files:** Modify stub `internal/api/systemstats.go`; tests; fixtures `testdata/contract/jobs/system_stats.json`

**Interfaces:** Produces `GET /api/system-stats` (`current_workspace`): `{"proxy_pool": ..., "rate_limiter": ..., "jobs": {"total", "pending", "running", "done", "failed"}}` where `jobs` counts the newest 100 ledger rows of the workspace (Python: `get_jobs(limit=100)`, counts of the four statuses; `cancelled` is in `total` only) and the first two objects come from `pyworker` unary `POST /internal/leadgen/system-stats` with `SYSTEM_STATS_RPC_TIMEOUT_SECONDS` (3 s); on RPC failure the zeroed shapes (`proxy_pool`: `total, http, socks5, socks4, blocked, domain_assignments` = 0; `rate_limiter`: `tracked_domains` 0, `tripped_domains` []) are returned with a `X-Stats-Source: degraded` response header (deviation D5: Python cannot fail here because it is in-process).

- [ ] **Step 1: Write failing tests**: `TestSystemStatsMatchesFixture` (fake pyworker replaying the recorded RPC answer), `TestJobCountsWindowOf100`, `TestCancelledOnlyInTotal`, `TestPyWorkerDownDegradedShape`, `TestPyWorkerSlowTimesOut`, `TestSystemStatsTenantJobsOnly`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): system stats route (RED)`, `feat(go): system stats route`.

### Task 18: Lead utility routes (score, verify-email, tech-stack)

**Files:** Modify stub `internal/api/leadutils.go`; tests; fixtures `testdata/contract/leadutils/`

**Interfaces:** Consumes Task 8, plan 06 `emailverify.Cascade`, `providers.Get("tech_stack")`. Produces `POST /api/leads/score` (`current_workspace`: `lead_id` first -> `LeadsByIDs` else inline `lead` object else 400 `provide lead_id or lead`; 404 `Lead not found`; `{"lead_id","company","score","tier"}`; inline bodies with wrong types answer 422 instead of Python's 500), `POST /api/leads/verify-email` (60/min; `email.strip()`, no `@` or contains a space -> 400 `email must be a single address`; `{"email","status","valid","deliverable","confidence","source","detail"}`; `valid` = status `valid`; cascade is workspace-scoped), `POST /api/leads/tech-stack` (30/min; empty domain 400 `domain is required`; scheme added; `website_fetch_disabled` -> 409 `website fetches are disabled; set TECH_STACK_WEBSITE_FETCH_ENABLED=1`; `{"domain","success","technologies","fields","error"}`; SSRF-guarded by the provider).

- [ ] **Step 1: Write failing tests**: `TestLeadUtilsMatchFixtures`, `TestScoreByIDVsInline`, `TestScoreNeitherIs400`, `TestScoreMissingLead404`, `TestScoreInlineWrongTypes422`, `TestVerifyEmailValidation400`, `TestVerifyEmailCascadeWorkspaceScoped`, `TestVerifyEmailRateLimit`, `TestTechStackDisabled409`, `TestTechStackRefusesPrivateHost`, `TestTechStackRateLimit`, `TestUtilsRequireWorkspaceMember`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): lead utility routes (RED)`, `feat(go): lead score route`, `feat(go): verify-email and tech-stack routes`.

### Task 19: Domain intel route

**Files:** Modify stub `internal/api/domainintelroute.go`; tests; fixtures `testdata/contract/leadutils/domain_intel_*.json`

**Interfaces:** Produces `POST /api/leads/domain-intel` (`require_editor`; body is an arbitrary JSON object): `domain` missing, non-string or blank -> 400 `domain is required`; `Clean` returns empty -> 400 `domain must be a valid domain name or URL`; otherwise `Analyze` result verbatim (key order). A non-object body answers 422 (hardening; Python 500s on `.get`). Analysis failure inside `Analyze` produces the partial result Python returns, never a 500.

- [ ] **Step 1: Write failing tests**: `TestDomainIntelMatchesFixtures`, `TestDomainIntelValidation400Variants`, `TestDomainIntelNonObjectBody422`, `TestDomainIntelViewer403`, `TestDomainIntelPartialResultOnDNSFailure`, `TestDomainIntelNoPrivateHostFetch` (domain `localhost`, `10.0.0.1`, `[::1]` are analysed via DoH only and never fetched directly).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): domain intel route (RED)`, `feat(go): domain intel route`.

### Task 20: source_health_check Go handler and bootstrap

**Files:** Create `internal/leadjobs/{health_job,handlers}.go`, tests; modify `cmd/worker/main.go` (one registration line, guarded by `GO_EXECUTOR_SOURCE_HEALTH`)

**Interfaces:** Consumes Task 12, Task 5 RPC, plan 06 `queue.EnqueueOnce`. Produces `func HandleSourceHealthCheck(ctx, d Deps, job queue.Job) error` = `handle_source_health_check`: master flag off (`SOURCE_HEALTH_ENABLED`) -> no-op and **no reschedule**; list sources (RPC `source-list`), probe in batches through `source-probe` (batch size = `SOURCE_HEALTH_PROBE_CONCURRENCY` default 8; the 1 s sleep lives in Python); a `null` yield or an RPC failure counts as yield 0 **and** marks the whole run as an outage (Python: a raised probe is yield 0; Go is stricter so an unreachable worker can never auto-disable sources, D6); `outage = zeroRatio >= SOURCE_HEALTH_OUTAGE_RATIO` (default 0.6); `ApplyRun` in one transaction; reschedule with `EnqueueOnce` (`_next_run_at` cadence ported, fire key as `_enqueue_health_if_absent`); `func BootstrapSourceHealth(ctx, tx) (bool, error)` for the scheduler. Cancellation: context cancel aborts the RPC and applies nothing. Lease/heartbeat via the existing queue helpers; executor fencing as in the first-worker plan (a Go and a Python worker never run the same job).

- [ ] **Step 1: Write failing tests**: `TestFlagOffNoopNoReschedule`, `TestOutageGuardSuppressesZeroTransitions`, `TestProbeRPCFailureIsOutageNotZero`, `TestBatchesRespectConcurrency`, `TestTransitionsPersistedAtomically`, `TestRescheduleSingleFlight` (two concurrent handlers, one next job), `TestCancelAppliesNothing`, `TestBootstrapIdempotent`, `TestGoAndPythonNeverBothClaim` (executor routing).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): source health job (RED)`, `feat(go): source_health_check handler`, `feat(go): source health bootstrap and worker registration`.

### Task 21: Settings sources routes

**Files:** Modify stub `internal/api/settingssources.go`; tests; fixtures `testdata/contract/sources/`

**Interfaces:** Consumes Tasks 12, 13 and plan 03 settings getter/setter, `identity.RequireGlobalAdmin`. Produces `GET /api/settings/sources` (array of `{id,name,icon,description,enabled,strategy,category,health}`; `health` is `{state,last_yield,last_ok_at,disabled_reason,manual_override}` or `null`; a health-layer error degrades to `null` badges like Python's `except`), `GET /api/settings/sources/health` (`Summary`; on error `{"total": 0, "sources": [], "error": <generic>}`), `PUT /api/settings/sources/{source_id}?enabled=true` (global admin; unknown id 404 `Source '<id>' not found`; sets `SOURCE_<ID>_ENABLED` to `1`/`0`; enabling also `ManualEnable`, errors from the health layer swallowed; `{"id","enabled"}`), `POST /api/settings/sources/{source_id}/health/reset` (global admin; `{"name","reset":false,"detail":"no health row"}` when absent; otherwise the row), `GET /api/settings/sources/registry` (`Registry()`).

- [ ] **Step 1: Write failing tests**: `TestSourcesMatchFixtures`, `TestSourcesHealthBadgeNullWithoutRow`, `TestSourcesHealthErrorShape`, `TestPutUnknownSource404`, `TestPutNonAdmin403`, `TestPutEnableClearsAutoDisabled`, `TestPutDisableDoesNotTouchHealth`, `TestResetNoRowShape`, `TestRegistryMatchesPython`, `TestEnabledQueryParamDefaultTrue`, `TestSourcesRouteOrdering` (`health` and `registry` never captured by `{source_id}`).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): settings sources routes (RED)`, `feat(go): settings sources routes`.

### Task 22: Coverage, cross-stack tests, nginx, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf` (one delimited section), `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations)
- Create: `testdata/contract/retained_leadgen_routes.txt`, `docs/go-port/runbooks/leadgen-cutover.md`, `internal/api/leadgen_coverage_test.go`, `internal/api/leadgen_crossstack_integration_test.go`, `legacy/python/tests/test_go_enqueued_collect.py`

**Interfaces:** Consumes everything. Produces: nginx blocks per route (retained paths first, always `api_backend`: `/api/lead/*/enrich`, `/api/v2/search/unified`, `/api/v2/scraper/scrape`, plan 06 `/sources/*/preview`; ported paths follow the canary bucket, **sticky per workspace** for `POST /api/collect`, `/api/jobs*` so ledger writes never split across stacks that read different backends; there is only one backend after the flip, stickiness is defence in depth). Cross-stack tests: a Go-enqueued `collect` row is claimed by a Python worker (`test_go_enqueued_collect.py` runs `handle_collect` against a fake `JobRunner` and asserts payload keys, ledger status transitions `pending -> running -> done`, and that `_is_cancelled` sees a Go cancel); a Python-enqueued row is readable by Go routes; both stacks list the same jobs byte-identically. Runbook, in order: (1) prerequisites (plans 01-06 cutovers done, Redis, `PY_WORKER_URL`/`PY_WORKER_TOKEN`, the Alembic revision on all Python workers); (2) **quiesce**: stop new collects (nginx 503 on `POST /api/collect` and `/jobs/*/retry` for the window), wait until no `collect`/`bulk_enrich` queue row is `pending`/`processing` (query in the runbook); (3) `migrate_collection_ledger.py --dry-run` then real run, `verify` must report zero mismatches, marker written; (4) flip `SQLITE_STORES_BACKEND=postgres` for API and Python worker together (the worker refuses to start with a mismatching marker); (5) enable `GO_COLLECTION_ROUTES_ENABLED` at 0% -> 5% -> 25% -> 100%; (6) later, `GO_EXECUTOR_SOURCE_HEALTH` after one clean Python-run cycle; rollback table (routes: nginx to 0%, safe; ledger: flip the backend flag back and run `migrate_collection_ledger.py --reverse` to copy rows written since the cutover back to the SQLite files, documented as the only data-moving rollback; do not roll back after more than 24 h without running it); metrics to watch (collect 5xx, ledger rows stuck `running` older than the queue timeout, queue rows without a ledger row and vice versa: the two reconcile queries are in the runbook).

- [ ] **Step 1: Write failing tests** `TestEveryLeadgenRouteIsPortedOrRetained` (parses `routers/leads.py` job/utility routes and `routers/settings.py` sources routes: every route is in Go patterns, in plan 04/05/06 lists or in `retained_leadgen_routes.txt`; expected 19 ported + 3 retained here), `TestRetainedRoutesNotRegisteredInGo`, `TestNoGoHandlerImportsScraperDeps`, `TestEveryJobTypeHandledOrPython` (`collect`, `bulk_enrich`, `source_workbook` dispatch to Python; `source_health_check` to Go only behind its flag), the cross-stack integration tests above.
- [ ] **Step 2: Run** `make test test-integration` and `uv run pytest tests/test_go_enqueued_collect.py` -> FAIL. **Step 3:** record any missing fixtures, write the nginx blocks, runbook and deviations (D1-D6, key-order note for pre-`_migrate` SQLite files, `/jobs/{id}/leads` id tiebreak, per-replica `system-stats`). **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (retained paths always `py`; `/api/collect` and `/api/jobs/x` follow the bucket) -> PASS.
- [ ] **Step 5: Commit** `test(go): leadgen route coverage and cross-stack (RED)`, `feat(infra): nginx split for leadgen collection`, `docs(go-port): leadgen cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row 13 "collect, bulk_enrich, source_workbook, source_health, scoring, store": `collect` API and ledger (2-4, 6, 10, 14-17), `bulk_enrich` enqueue is plan 05 (Go) and its handler stays Python (kept `executor='python'`; Global Constraints), `source_workbook` handler stays Python (enqueue is plan 06), `source_health` (5, 12, 20, 21), scoring (8, 18), store: job ledger (2-4, 10) while the lead store is plans 04/05 and `llm_usage` already Postgres; HTTP providers for leads utilities are plan 06's (`emailverify`, `techstack`), `domain_intel` here (13, 19); progress (11). Scrapers, browser tier, proxy pool, rate limiter, staffspy/jobspy, ddgs: Python (only `system-stats` and `source-probe` RPCs reach them).
- **Consistency:** `collectledger` (10) used by 14-17, 22; `leadprogress.Bus` (11) by 14, 15; `leadscore` (8) by 18; `collectintent` (9) by 14; `sourcehealth` (12) by 20, 21; `sourcecatalog` (13) by 21; `queue.EnqueueOnce` (plan 06) by 14, 16, 20.
- **Review Focus:** each line has its tests in Tasks 3, 4, 22 (1); 10, 14, 16 (2); 10, 14, 15 (3); 8, 9 (4); 12, 20 (5); 17 (6).
- **Proportion:** interfaces, key orders, error strings, test names and file ownership carry the decisions; query and handler bodies are left to the implementer and to the recorded fixtures.

## Open decisions (defaults apply unless overridden)

- **D1 Ledger destination.** Default (confirm): new Postgres tables `collection_jobs` and `collection_job_stages` (named to avoid the queue's `jobs`), SQLite value shapes preserved (text timestamps), Python repointed behind `SQLITE_STORES_BACKEND`, one-time backfill with a quiesced cutover and a reverse-copy rollback. Alternative rejected: keep the SQLite ledger and read it from Go (impossible per file, RLS-less, no cross-process atomicity).
- **D2 Atomic enqueue.** Default: ledger row and queue row in one Postgres transaction (Python compensates after failure with a `failed` row and 503; Go never creates the orphan). Documented deviation.
- **D3 `decide_collection` in Go.** Default: ported with 300 vectors because `/api/collect` needs it synchronously and it is pure regex logic. Fallback if any pattern is not RE2-compatible: the one function goes behind a unary Python RPC and the plan's other tasks are unchanged.
- **D4 Routes left on FastAPI.** `POST /api/lead/{id}/enrich` (scrapers, SSE), `GET /api/v2/search/unified` (academic source registry, unrelated to collection), `POST /api/v2/scraper/scrape` (browser-tier scraper). Default: retained, listed in `retained_leadgen_routes.txt`. They can be fronted by Go later through `pyworker` once the scraper worker contract (plan 13) is fixed.
- **D5 Process-local stats.** Default: `system-stats` proxy and limiter numbers come from the Python replica the RPC reaches and degrade to zeros with a header when unreachable; no shared state is introduced for them.
- **D6 Source-health strictness.** Default: a failed or null probe batch from the Python worker is an outage run (no zero transitions), stricter than Python, so a Python worker outage can never auto-disable sources. `SOURCE_HEALTH_ENFORCE` and `SOURCE_HEALTH_ENABLED` keep their Python meaning and defaults (observe-only).
- **D7 `bulk_enrich` and `collect` handlers.** Default: stay `executor='python'` indefinitely (they drive scrapers, email finder and website scrapers); Go only enqueues. A later plan may move `bulk_enrich`'s `find_emails` action to Go over plan 06's providers.
- **D8 Rollout.** Default: all new Go paths default off (`GO_COLLECTION_ROUTES_ENABLED`, `GO_EXECUTOR_SOURCE_HEALTH` false); canary by workspace cohort with sticky `/collect` and `/jobs*`; the ledger flip is a coordinated, quiesced step, not a canary.

## User decisions

(none yet; the defaults above apply until the user objects)
