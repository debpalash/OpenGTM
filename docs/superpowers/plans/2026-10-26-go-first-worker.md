# Go Backend — Baseline + First Go Worker (M0–M2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Measure the current Python enrichment throughput, land the cheap Python fixes, and — only if the baseline still misses the target — ship one Go worker that executes declarative-HTTP-provider `run_workbook` jobs behind a workspace-cohort flag with exercised rollback.

**Architecture:** The Go worker is a second executor on the existing PostgreSQL `jobs` queue. A new `jobs.executor` column routes each job to exactly one executor; a `jobs.lease_token` column fences heartbeats and commits. Python decides eligibility at enqueue time and embeds the already-verified manifests in the payload, so Go never loads YAML or verifies signatures. Go writes the same rows (cells, row overlay, spend attempts, receipts) and the same Redis events as Python. Python stays the only migration owner and the only stale-claim reaper.

**Tech Stack:** Go 1.27.3 (`go 1.27` + `toolchain go1.27.3`), pgx v5, sqlc, go-redis v9, `log/slog`, `net/http` with a guarded pooled transport; existing Python 3 / FastAPI / SQLAlchemy / Alembic / pytest / uv; PostgreSQL 18 in CI (Compose currently runs 16 — Go must work on both).

**Spec:** GitHub issue #33 (“RFC: Go backend rewrite with Python specialist workers and optional Rust acceleration”), plus the 2026-10-13 status comment on that issue (deferred until baseline). Read both before starting.

**Layout note (branch `go-rewrite`):** The Python backend now lives **untracked** at `legacy/python/` (gitignored), with its original relative layout. Every Python path in this plan (`apps/api/...`, `tests/...`, `migrations/...`, `scripts/*.py`) means `legacy/python/<path>`. Run Python commands from `legacy/python/`. Python edits are not versioned by this repo, so keep a backup or a separate repo for them.

## Global Constraints

- Go pinned in `go.mod` as `go 1.27` + `toolchain go1.27.3`; pin every module version; pin image digests in Compose at release.
- Database access only through pgx v5 + sqlc-generated queries; generated code is committed; CI fails on regeneration drift.
- Runtime DB role stays non-superuser and `NOBYPASSRLS`. Every tenant query runs inside a transaction that first executes `SELECT set_config('app.workspace_id', $1, true)`. No tenant query outside such a transaction. Missing workspace ID fails closed.
- One schema-migration owner: the existing Alembic `migrate` service. Go never runs DDL.
- Secrets never appear in job payloads, logs, traces, fixtures, or error strings. Provider credentials are never shared across workspaces.
- Bounded everything: goroutines per job (= payload `concurrency`, default and max taken from `DEFAULT_CONCURRENCY` in `apps/api/services/workbook/enrichment.py`), DB pool (`GO_WORKER_DB_MAX_CONNS`, default 10), response body (`1 MiB`), redirects (`0` — never follow, matching httpx's default), request timeout (manifest `request.timeout`, `0 < t ≤ 120s`).
- Behaviour parity with Python is the release bar: identical cell values/statuses/errors, row overlay JSON, spend-attempt rows and contract hashes, receipts, and Redis event payloads (modulo timestamps and generated IDs).
- Feature flag `GO_EXECUTOR_ENABLED` defaults to `false`; cohort `GO_EXECUTOR_WORKSPACES` (comma-separated workspace IDs) defaults to empty.
- Every commit is DCO signed (`git commit -s`) per `CONTRIBUTING.md`.
- No gRPC/Protobuf, `packages/contracts`, or Rust in this plan (YAGNI until M4 / a proven hotspot).

## Review Focus

1. **Canonical-JSON hash drift.** The spend `contract_hash` and the `attempt_key` are SHA-256 hashes of Python `json.dumps` output. Python escapes non-ASCII as `\uXXXX`, does not escape `<>&`, and prints floats with `repr`. If Go's hash differs, a rollback to Python sees `contract_conflict` and refuses to bill or re-run. Pinned in Task 9 with golden vectors that include non-ASCII names, `<`, `&`, floats, and nested objects.
2. **A pre-upgrade Python worker claims a Go row during a rolling deploy.** An old worker's claim SQL has no `executor` filter. Pinned in Task 6: the Go worker refuses to start below the required Alembic revision, the enqueue path only emits `executor='go'` when the flag is on, and `docs/go-executor.md` requires all Python workers to be upgraded before the flag is turned on.
3. **The workspace GUC leaks across pooled connections.** If Go ever runs a tenant query outside the scoped transaction, a pooled connection can carry another tenant's `app.workspace_id`. Pinned in Task 5 with a pool of size 1 alternating between two tenants under FORCE RLS: tenant A must never see tenant B's rows, and an unscoped query must return zero rows.
4. **Cancellation mid-waterfall.** Once the API commits `cancelled`, Go must not start another provider call, must not overwrite `cancelled` with `completed` or `failed`, and must leave a dispatched paid call `uncertain` rather than `released`. Pinned in Task 12.
5. **SSRF through redirects, DNS rebinding, or the test allowlist.** Go's `net/http` follows redirects by default, and a hostname can re-resolve to a private IP at dial time. The simulator allowlist must also never take effect in production. Pinned in Task 8 with a dial-time IP check, no redirect following, and an allowlist that is ignored unless `APP_ENV` is `test` or `bench`.

---

## File Structure

```
apps/backend-go/
  go.mod, go.sum, sqlc.yaml, Makefile
  cmd/worker/main.go            # wiring, signals, schema gate
  cmd/providersim/main.go       # deterministic provider simulator (M0)
  internal/config/config.go     # env → Config
  internal/db/schema.sql        # generated from alembic head (scripts/go_schema_dump.sh)
  internal/db/queries/*.sql     # sqlc inputs, one file per domain
  internal/db/gen/              # sqlc output (committed)
  internal/tenancy/tx.go        # InWorkspace(ctx, pool, ws, fn)
  internal/queue/{claim,lease,finalize}.go
  internal/manifest/{types,template,project,normalize}.go
  internal/egress/{guard,transport}.go
  internal/canonjson/canonjson.go
  internal/spend/{identity,ledger}.go
  internal/credentials/client.go
  internal/progress/redis.go
  internal/workbook/{run,cell,overlay,receipt,stats}.go
  testdata/                     # golden vectors + parity fixtures (no secrets)
apps/api/services/go_executor.py             # eligibility + payload builder
apps/api/routers/internal_credentials.py     # service-token credential endpoint
migrations/versions/<rev>_jobs_executor_routing.py
scripts/bench/enrichment_bench.py            # M0 harness
scripts/go_schema_dump.sh
docs/go-executor.md                          # rollout + rollback runbook
docs/benchmarks/<date>-baseline.md           # M0 report
tests/test_go_executor_eligibility.py
tests/test_go_parity.py                      # postgres + go marker
```

---

## Part A — M0: Baseline and cheap Python fixes

### Task 1: Deterministic provider simulator + test-only egress allowlist

**Files:**
- Create: `apps/backend-go/go.mod`, `apps/backend-go/cmd/providersim/main.go`, `apps/backend-go/cmd/providersim/main_test.go`
- Create: `tests/fixtures/manifests_bench/email/sim_email.yaml`
- Modify: `apps/api/core/url_guard.py` (allowlist hook in `check_url`)
- Test: `tests/test_url_guard_bench_allowlist.py`

**Interfaces:**
- Produces: `providersim` HTTP server. `POST /email-finder` returns `{"email": "<first>.<last>@<domain>"}`. Its behaviour is set by the `SIM_PROFILE` env var: `ok` | `slow` (p50 300ms) | `ratelimit` (every 10th call → 429) | `timeout` (every 20th call hangs 30s) | `malformed` (every 15th call → non-JSON). Latency and failures are seeded by `SIM_SEED`, so the same seed always gives the same sequence. `GET /stats` returns `{"calls": N, "by_status": {...}}`.
- Produces: `EGRESS_ALLOW_HOSTS` env var (comma-separated hostnames). It is honoured only when `APP_ENV ∈ {test, bench}`; Task 8's Go guard uses the same rule.

- [ ] **Step 1: Write failing tests**: `test_allowlisted_host_permitted_in_bench` (APP_ENV=bench, EGRESS_ALLOW_HOSTS=providersim → `check_url("http://providersim:8099/x", resolve=False)` returns the URL); `test_allowlist_ignored_in_production` (APP_ENV=production, same env → raises `BlockedUrlError`); `test_allowlist_ignored_when_unset`. Go: `TestSimDeterministic` — two runs with `SIM_SEED=7 SIM_PROFILE=ratelimit` produce identical status sequences over 100 calls.
- [ ] **Step 2: Run** `uv run pytest tests/test_url_guard_bench_allowlist.py -q` and `cd apps/backend-go && go test ./cmd/providersim/...` → FAIL.
- [ ] **Step 3: Implement** the allowlist check as the first branch of `check_url` (exact hostname match only, no wildcards) and the simulator. The bench manifest is the same shape as `leadmagic.yaml`, with `url: http://providersim:8099/email-finder`, `auth.type: none`, `cost_per_lookup: 0.05`.
- [ ] **Step 4: Run both** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "test(bench): deterministic provider simulator and bench-only egress allowlist"`

### Task 2: Enrichment completion harness

**Files:**
- Create: `scripts/bench/enrichment_bench.py`, `scripts/bench/README.md`
- Modify: `docker-compose.yml` — add `providersim` under profile `bench` only
- Test: `tests/test_enrichment_bench_harness.py`

**Interfaces:**
- Produces: CLI `uv run python scripts/bench/enrichment_bench.py --workspaces W --rows R --columns C --rate {100,300,1000} --duration 1800 --warmup 120 --profile ok|slow|ratelimit|timeout|malformed --out report.json`.
- A **completion** is one row whose selected columns have all reached a terminal cell status (`complete`/`error`/`skipped`), with the `WorkbookEnrichment` row, the `WorkbookRow.enrichments` overlay, and any spend attempt committed. No-match (`error=no_data`) results are counted separately from successes.
- Report JSON keys (exact): `commit, hardware, config{WORKER_CONCURRENCY, WORKER_MAX_ACTIVE_PER_WORKSPACE, DEFAULT_CONCURRENCY, replicas}, workload, completions_per_sec, success_per_sec, no_match_per_sec, queue_delay_ms{p50,p95,p99}, e2e_ms{p50,p95,p99}, cpu_pct, rss_mb, db{connections_max, lock_waits, writes}, provider{attempts, http_429, timeouts, errors}, backlog_end, fairness_gini, memory_bounded`.

- [ ] **Step 1: Write failing test** `test_report_schema_and_counts`: run the harness in `--dry-run` mode with `--rows 20 --columns 1` against SQLite and an in-process fake provider. Assert the report has every key above, `success + no_match == 20`, and `backlog_end == 0`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** The harness seeds workbooks through the public REST API, enqueues through the normal `run_workbook` path, and polls `jobs` and cell tables. Use `pg_stat_activity` and `pg_locks` sampled every 1s for the DB metrics, and `psutil` for worker CPU/RSS. `fairness_gini` is the Gini coefficient of completions per workspace.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "test(bench): enrichment completion harness"`

### Task 3: Cheap Python fixes from the 2026-10-13 comment

**Files:**
- Modify: `apps/api/services/leadgen/http.py` (`_fetch_plain`), `apps/api/services/leadgen/enrichment/declarative/compiler.py` (`DeclarativeProvider.enrich`)
- Modify: `apps/api/services/workbook/refresh.py` (`_stale_row_ids`)
- Modify: `docs/architecture.md` — add a "Worker capacity settings" table
- Test: `tests/test_http_client_reuse.py`, `tests/test_refresh_stale_sql.py` (reuse `tests/refresh_stale_support.py`)

**Interfaces:**
- Produces: `get_shared_client(*, proxy: str | None, timeout: float) -> httpx.AsyncClient`. One client per `(proxy, event loop)`, `follow_redirects=False`, closed on worker shutdown. Redirect validation stays in `guarded_get`.
- Produces: `_stale_row_ids(db, workbook_id, *, limit: int) -> list[int]` as one bounded SQL query (`LIMIT :limit`, default `REFRESH_STALE_BATCH=500`) that returns the same IDs as today's in-memory version.

- [ ] **Step 1: Failing tests**: `test_fetch_plain_reuses_client` (monkeypatch `httpx.AsyncClient.__init__` counter; 50 fetches → 1 construction); `test_declarative_reuses_client`; `test_proxy_clients_not_shared` (two proxies → two clients); `test_stale_rows_sql_matches_legacy` (seed fixture; new == old ids, sorted); `test_stale_rows_bounded` (limit=10 → ≤10).
- [ ] **Step 2: Run** `uv run pytest tests/test_http_client_reuse.py tests/test_refresh_stale_sql.py -q` → FAIL.
- [ ] **Step 3: Implement.** Keep the per-call SSRF `check_url(..., resolve=True)` before every request.
- [ ] **Step 4: Run** the new tests plus `uv run pytest -q -m 'not live and not postgres'` → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "perf(leadgen): reuse HTTP clients and bound stale-row selection"`

### Task 4: Record baseline and decide

**Files:**
- Create: `docs/benchmarks/2026-MM-DD-baseline.md`

- [ ] **Step 1:** On the dedicated bench host, run the harness at 100/300/1000 completions/s for each profile, before and after Task 3, with `WORKER_CONCURRENCY` at 1 and 8 and with 1 and 4 worker replicas. Record `nproc`, RAM, disk, the commit, and the full env.
- [ ] **Step 2:** Write the report: tables per load point, the bottleneck identified from the profiles (py-spy flame graph for the worker, `pg_stat_statements` top 10), and proposed numerical SLOs for p95 e2e and queue delay.
- [ ] **Step 3: Decision gate (human).** If the tuned Python run sustains the target on the documented workload with bounded backlog and memory, **stop here**: post the report to issue #33 and close M1–M2 as not needed. Otherwise record which load point failed and why, then continue to Part B.
- [ ] **Step 4: Commit** `git commit -s -m "docs(bench): M0 baseline report"`

---

## Part B — M1: Foundations

### Task 5: Go module skeleton, schema dump, sqlc, tenant transaction helper

**Files:**
- Create: `apps/backend-go/{sqlc.yaml,Makefile}`, `internal/config/config.go`, `internal/db/schema.sql`, `internal/tenancy/tx.go`, `internal/tenancy/tx_test.go`
- Create: `scripts/go_schema_dump.sh`
- Modify: `.github/workflows/ci.yml` — add `go` job

**Interfaces:**
- Produces: `config.Load() (Config, error)`. Fields: `DatabaseURL, RedisURL, InternalAPIURL, InternalServiceToken, WorkerConcurrency (default 4, max 64), DBMaxConns (default 10), ShutdownGrace (default 30s, max 300s), HeartbeatInterval (30s), AppEnv, EgressAllowHosts`. It rejects an empty `InternalServiceToken`, and it rejects `EgressAllowHosts` when `AppEnv` is not `test` or `bench`.
- Produces: `tenancy.InWorkspace(ctx context.Context, pool *pgxpool.Pool, workspaceID string, fn func(pgx.Tx) error) error`. It returns `ErrNoWorkspace` for an empty ID, begins the transaction, sets `set_config('app.workspace_id', $1, true)`, runs `fn`, and commits (or rolls back on error).
- Produces: `scripts/go_schema_dump.sh` — runs `alembic upgrade head` on a scratch DB and `pg_dump --schema-only --no-owner --no-privileges` → `internal/db/schema.sql`.
- Telemetry (RFC M1): JSON `log/slog` with `job_id`, `workspace_id`, `lease_token` on every job log line, and `net/http/pprof` plus a `/metrics` text endpoint on `127.0.0.1:6060` only. The OTel SDK and trace propagation are deferred to M3, since M2 has no cross-service calls except the credential fetch.

- [ ] **Step 1: Failing tests** (`//go:build integration`, need `TEST_DATABASE_URL`, run as the `app_rls_test` role created by `tests/pg_rls_support.py` conventions): `TestInWorkspaceIsolatesTenants` (pool MaxConns=1; insert a workbook in A, then read inside B → 0 rows, then read inside A → 1 row, repeated 50× alternating); `TestUnscopedQueryFailsClosed` (a raw `pool.Query` on `workbooks` → 0 rows); `TestEmptyWorkspaceRejected`; `TestRollbackOnError`.
- [ ] **Step 2: Run** `cd apps/backend-go && go test -race -tags integration ./internal/tenancy/...` → FAIL.
- [ ] **Step 3: Implement.** CI `go` job: Postgres 18 service, `uv run alembic upgrade head`, `scripts/go_schema_dump.sh`, `git diff --exit-code internal/db/schema.sql internal/db/gen`, `sqlc diff`, `go vet ./...`, `go test -race ./...`, `go test -race -tags integration ./...`.
- [ ] **Step 4: Run** → PASS locally and in CI.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): module skeleton, sqlc schema pipeline, tenant transaction helper"`

### Task 6: Executor routing + lease token (additive migration, Python taught first)

**Files:**
- Create: `migrations/versions/<rev>_jobs_executor_routing.py` (`down_revision` = output of `uv run alembic heads`)
- Modify: `apps/api/models.py` (`Job`), `apps/api/services/queue_service.py` (`_eligible_clause`, both claim paths, `_recover_attempts`, `_transition_dead_jobs`, `_process_job` finalize, `_claim_is_active`, `_renew_claim`, `_load_claimed`)
- Modify: `apps/api/services/workbook/batch_attempts.py` (`_owned_job` adds the lease-token predicate when the owner carries one)
- Test: `tests/test_queue_executor_routing.py`, extend `tests/test_queue_claim_pg.py`, `tests/test_queue_lease_recovery.py`

**Interfaces:**
- Schema: `jobs.executor TEXT NOT NULL DEFAULT 'python' CHECK (executor IN ('python','go'))`; `jobs.lease_token BIGINT NOT NULL DEFAULT 0`; index `ix_jobs_claim (executor, status, priority DESC, next_run_at)` with `WHERE status='pending'`.
- Python: `QueueService(executor: str = "python")`. Both claims add `AND j.executor = :executor` and set `lease_token = lease_token + 1`. `_load_claimed` returns a `lease_token` key. Every lease predicate (heartbeat, finalize, `_claim_is_active`, reaper CAS) adds `Job.lease_token == <claimed token>`. The `__queue_lease` payload for `run_workbook` gains `"lease_token": int`.
- Reaper: Python's `_recover_attempts` keeps recovering stale `processing` rows for **all** executors, using CAS on `lease_token`. It does not change `executor`.
- Constant: `REQUIRED_GO_SCHEMA_REVISION = "<rev>"`, exported in `apps/api/services/go_executor.py` and duplicated in `apps/backend-go/internal/config/config.go`. CI asserts that the two match.

- [ ] **Step 1: Failing tests**: `test_python_never_claims_go_rows` (insert one go row and one python row → the Python claim returns only the python row; a second claim returns None); `test_claim_increments_lease_token`; `test_stale_finalize_rejected_after_reclaim` (claim → force a reaper recovery → reclaim → the first attempt's finalize changes 0 rows and the status stays the second attempt's); `test_reaper_recovers_go_rows_and_keeps_executor`; `test_heartbeat_rejected_with_old_token`. PG variants live in `test_queue_claim_pg.py`, with 8 concurrent claimers over 200 mixed rows → no duplicates, and each executor gets only its own rows.
- [ ] **Step 2: Run** `uv run pytest tests/test_queue_executor_routing.py tests/test_queue_lease_recovery.py -q` and `TEST_DATABASE_URL=... uv run pytest -q -m postgres tests/test_queue_claim_pg.py` → FAIL.
- [ ] **Step 3: Implement** the migration (upgrade and downgrade) and the Python changes. SQLite keeps the guarded-UPDATE path, now with the executor predicate.
- [ ] **Step 4: Run** the new tests plus the full non-live suite → PASS. Then `alembic downgrade -1 && alembic upgrade head` on a PG scratch DB → clean.
- [ ] **Step 5: Commit** `git commit -s -m "feat(queue): executor routing and lease-token fencing"`

### Task 7: Go queue — claim, heartbeat, finalize, schema gate, shutdown

**Files:**
- Create: `internal/db/queries/jobs.sql`, `internal/queue/{claim,lease,finalize}.go` + `_test.go`, `cmd/worker/main.go`

**Interfaces:**
- Consumes: `tenancy.InWorkspace`, `config.Config`, schema from Task 6.
- Produces: `type Claim struct { ID int64; Type string; WorkspaceID string; Payload json.RawMessage; WorkerID string; LockedAt time.Time; LeaseToken int64 }`.
- `queue.ClaimNext(ctx, pool, workerID string, tenantCap int) (*Claim, error)` — the same SQL shape as `_claim_next_job_postgres`: eligibility, tenant-cap ordering, `FOR UPDATE SKIP LOCKED LIMIT 1`, `pg_advisory_xact_lock(hashtext(ws))` recheck, at most 100 retries. It adds `executor='go' AND type='run_workbook'`. Rows of any other `type` with `executor='go'` are finalized `failed` with `error='go executor: unsupported job type'` (an explicit failure rather than an endless loop).
- `queue.Heartbeat(ctx, pool, c *Claim) (active bool, err error)` — updates `last_heartbeat` only where `(id, worker_id, locked_at, lease_token, status='processing')` match. `active=false` means the lease was lost: the caller cancels the job context.
- `queue.Finalize(ctx, pool, c *Claim, outcome Outcome) (persisted bool, err error)` where `Outcome{Status: "completed"|"failed"; Error string}`. It is a direct port of the finalize block in `_process_job` (lines 532–598): it takes `SELECT ... FOR UPDATE` on the lease predicate, keeps a committed `cancelled`, retries with backoff `2^(retry_count)` minutes up to `max_retries` (same retry ceiling as Python), and clears `worker_id`/`locked_at`.
- `queue.IsCancelled(ctx, pool, c *Claim) (bool, error)`, called at most every 2s per job (matching Python's throttle).
- Worker main: refuses to start unless `SELECT version_num FROM alembic_version` equals `REQUIRED_GO_SCHEMA_REVISION`. Runs `WorkerConcurrency` slots. On SIGTERM it stops claiming, waits up to `ShutdownGrace`, then cancels job contexts and leaves the rows for the Python reaper.

- [ ] **Step 1: Failing integration tests**: `TestGoClaimsOnlyGoRows`; `TestTenantCapRespected` (cap 2, 5 jobs for workspace A + 1 for B → B is claimed before A's 3rd); `TestFinalizePreservesCancelled`; `TestFinalizeRetryBackoff` (retry_count 0 → pending, next_run_at ≈ now+1m; at max_retries → failed with `Final Failure:` prefix); `TestFinalizeAfterLeaseLossIsNoop`; `TestHeartbeatDetectsLeaseLoss`; `TestSchemaGateRefusesOldRevision`; `TestUnsupportedGoJobTypeFails`. Cross-executor: `tests/test_go_queue_interop.py` (postgres marker, builds and runs the Go worker binary) — Python reaper recovers a Go claim killed with SIGKILL; a restarted Go worker reclaims it with `lease_token` +1.
- [ ] **Step 2: Run** `go test -race -tags integration ./internal/queue/...` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** → PASS, including the Python interop test.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): queue claim, lease, finalize with cross-executor fencing"`

---

## Part C — M2: First Go job — declarative HTTP waterfall `run_workbook`

### Scope decision (made here so the implementer doesn't have to)

M2's Go executor handles a `run_workbook` job **only when Python's eligibility check passes**. Anything else stays on Python. A job is eligible only if:
- `GO_EXECUTOR_ENABLED=true` and the workspace is in `GO_EXECUTOR_WORKSPACES`;
- every selected column has `type ∈ {enrichment, waterfall}`, no `condition`, and a `target_field` that is not in `STRUCTURED_FIELDS`;
- every provider in every column's chain is a loaded declarative manifest whose `response.mappings` keys are exactly `{target_field}` (so `_write_back_to_lead` is never triggered);
- email-target columns have `verify: false` (the verify cascade stays on Python until M4);
- `PROVENANCE_TRACKING_ENABLED` and `AUTOMATIONS_ENABLED` are both off for the run (no provenance or row-change emits in M2);
- the payload has no AI batch prepass (implied by the column types).

### Task 8: Egress guard and pooled transport (Go)

**Files:**
- Create: `internal/egress/{guard,transport}.go` + `_test.go`, `testdata/url_guard_vectors.json`
- Create: `tests/test_url_guard_vectors.py` (asserts Python produces the same verdicts from the same JSON file)

**Interfaces:**
- `egress.CheckURL(raw string, allowHTTP bool) error` — a port of `url_guard.check_url` semantics: scheme check, blocked hostnames (`localhost, ip6-localhost, ip6-loopback, metadata.google.internal, metadata`), any-encoding IP parsing (decimal, octal, hex, dotted mixes), and blocking of private/loopback/link-local/reserved/multicast/unspecified/IPv4-mapped/metadata (`169.254.169.254`, `fd00:ec2::254`) addresses.
- `egress.NewClient(allowHosts []string) *http.Client`: one shared `http.Transport` with `MaxIdleConnsPerHost=64` and `IdleConnTimeout=90s`. `CheckRedirect` returns `http.ErrUseLastResponse`. `DialContext` resolves the host, rejects the dial if any resolved IP is blocked, and dials the vetted IP. The transport has no cookie jar, and auth is per request only.

- [ ] **Step 1: Failing tests**: the shared vector file (≥40 cases, including `http://2130706433/`, `http://0177.0.0.1/`, `http://0x7f.1/`, `http://[::ffff:10.0.0.1]/`, `http://169.254.169.254/`, public `https://api.leadmagic.io/`) — Go `TestCheckURLVectors` and Python `test_url_guard_vectors` must give identical verdicts. `TestRedirectNotFollowed` (httptest 302 → the client returns the 302). `TestDialTimeRebindBlocked` (a custom resolver returns a public IP for the check and `127.0.0.1` at dial → error). `TestAllowlistIgnoredOutsideBench`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): SSRF-guarded pooled egress client with vector parity"`

### Task 9: Canonical JSON + spend ledger (Go)

**Files:**
- Create: `internal/canonjson/canonjson.go` + `_test.go`, `internal/spend/{identity,ledger}.go` + `_test.go`, `internal/db/queries/spend.sql`
- Create: `testdata/canonjson_vectors.json`, generated by `scripts/gen_canonjson_vectors.py`, which calls Python's real `json.dumps(sort_keys=True, separators=(",", ":"), allow_nan=False)` and `json.dumps(..., separators=(",", ":"))`.

**Interfaces:**
- `canonjson.Marshal(v any, sortKeys bool) ([]byte, error)` — byte-identical to Python's `json.dumps(v, sort_keys=sortKeys, separators=(",", ":"), ensure_ascii=True, allow_nan=False)`. Requirements: `\uXXXX` escaping for every non-ASCII rune (surrogate pairs above U+FFFF), no escaping of `<>&`, Python `repr` float formatting (`0.05`, `1e-07`, `1.0`), NaN/Inf rejected, and `[]any`/`map[string]any`/`string`/`float64`/`int64`/`bool`/`nil` as the only accepted shapes.
- `spend.AttemptKey(ws, workbookID string, jobID int64, rowIdentity, columnID, provider string) string` — the port of `WorkbookExecutionIdentity.attempt_key` (unsorted list, `run_id = "job:<id>"`).
- `spend.Reserve(ctx, tx pgx.Tx, r Reservation) (Admission, error)`, `spend.Transition(ctx, tx, id Identity, action string) (bool, error)` with actions `dispatch|release_before_dispatch|mark_uncertain`, and `spend.Settle(ctx, tx, id Identity, chargedMicroUSD int64, basis string, result map[string]any) (SettleResult, error)`. These are direct ports of `reserve_attempt`, `transition_attempt` and `settle_attempt` in `apps/api/services/workbook/spend_service.py`. They keep the same reason strings (`workbook_not_found, contract_conflict, cell_budget, invalid_workbook_budget, workbook_budget, attempt_not_found, settlement_conflict, invalid_state`) and the same micro-USD rounding (exposure = ceil(cost×1e6); spent = ceil; cap = floor). `dispatch` also fences against the job lease: the `_owned_job` predicate with `lease_token`.
- `spend.AccountingEnvelope(provider string, resp ProviderResponse, estimatedMicroUSD int64) (Envelope, error)` — the port of `provider_accounting.accounting_envelope`, including `ErrUncertainAccounting`.

- [ ] **Step 1: Failing tests**: `TestCanonJSONVectors` (≥30 vectors: non-ASCII names `"José"`, `"北京"`, emoji, `"<a&b>"`, floats `0.05/0.1+0.2/1e-07/100.0`, nested maps, empty containers) → Go bytes == Python bytes; `TestAttemptKeyMatchesPython` (vectors from the same script); integration `TestReserveIdempotentSameContract`, `TestReserveConflictOnChangedInputs`, `TestCellBudgetEnforced`, `TestWorkbookBudgetEnforced`, `TestDispatchRequiresLiveLease`, `TestSettleExactlyOnce` (second settle with same amount → `reused`; different amount → `settlement_conflict`), `TestUncertainCannotBeReleased`. Cross-executor: `tests/test_go_spend_interop.py` — Go reserves, then Python's `reserve_attempt` with the same inputs returns `reused` (the hashes match).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): canonical JSON and spend ledger with Python hash parity"`

### Task 10: Manifest execution engine (Go)

**Files:**
- Create: `internal/manifest/{types,template,project,normalize}.go` + `_test.go`, `testdata/manifest_vectors.json` (generated by `scripts/gen_manifest_vectors.py` from Python's `render_string`, `render_template`, `project_value`, `project_response`, `normalize_fields`, `_lead_input_ctx`)

**Interfaces:**
- `manifest.Manifest` mirrors the Pydantic `ProviderManifest` JSON (`model_dump(mode="json")`). It is decoded from the payload, never from YAML.
- `manifest.InputContext(lead map[string]string) map[string]any` — the port of `_lead_input_ctx`, including `_domain_of` and the first/last name split.
- `manifest.Render(v any, ctx map[string]any, env func(string) string) any`, `manifest.Project(data any, mappings map[string]string) map[string]any`, and `manifest.Normalize(fields map[string]any) map[string]any`.
- `manifest.Execute(ctx context.Context, client *http.Client, m Manifest, lead map[string]string, cred func(envVar string) (string, error)) Result` with `Result{Success bool; Fields map[string]any; Confidence float64; DurationMS float64; Error string; HTTPStatus int}`. Error strings match Python exactly: `"<name>: missing <ENV>"`, `"blocked_url: <first 60 chars>"`, `"http_<code>"`, `"non_json_response"`, `"timeout"`, `"no_data"`, the vendor message truncated to 120 characters, and other errors truncated to 120. Bodies are read through `io.LimitReader(1 MiB)`, and oversize bodies give `non_json_response`.

- [ ] **Step 1: Failing tests**: `TestRenderVectors`, `TestProjectVectors` (fan-out `[]`, index, literal prefix, missing path), `TestNormalizeVectors`, and `TestInputContextVectors` all match Python. Execute against httptest: `TestExecuteSuccess`, `TestExecuteHTTP429`, `TestExecuteMalformed`, `TestExecuteTimeout`, `TestExecuteVendorErrorEnvelope`, `TestExecuteMissingKeyNoNetwork` (server hit count stays 0), `TestExecuteOversizeBody`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): declarative manifest engine with Python vector parity"`

### Task 11: Eligibility, payload v1, and internal credential endpoint (Python)

**Files:**
- Create: `apps/api/services/go_executor.py`, `apps/api/routers/internal_credentials.py`
- Modify: the `run_workbook` enqueue sites — find them with `grep -rn '"run_workbook"' apps/api --include=*.py`, and route every one through `enqueue_run_workbook`
- Modify: `apps/api/main.py` (mount router), `apps/api/core/config.py` (`GO_EXECUTOR_ENABLED`, `GO_EXECUTOR_WORKSPACES`, `INTERNAL_SERVICE_TOKEN`)
- Create: `internal/credentials/client.go` + `_test.go`
- Test: `tests/test_go_executor_eligibility.py`, `tests/test_internal_credentials.py`

**Interfaces:**
- `go_eligibility(db, workbook, payload: dict) -> tuple[bool, str]` — returns `(False, reason)` for the first failed rule in the Scope decision above. Reasons are stable snake_case strings such as `flag_off`, `not_in_cohort`, `column_type`, `condition`, `structured_target`, `non_declarative_provider`, `mapping_not_target_only`, `email_verify`, `provenance_on`, `automations_on`.
- `enqueue_run_workbook(db, payload: dict, *, priority: int = 1) -> Job` — sets `executor='go'` when eligible. When eligible it adds `payload["go"] = {"version": 1, "manifests": {name: manifest.model_dump(mode="json")}, "manifest_sha256": {name: sha}, "costs": {name: provider_cost(name)}, "default_concurrency": DEFAULT_CONCURRENCY}`. It logs the eligibility reason at INFO without payload contents.
- `GET /internal/v1/credentials/{env_var}` with headers `Authorization: Bearer <INTERNAL_SERVICE_TOKEN>` and `X-Workspace-Id`. It compares the token in constant time and only allows `env_var` values named by some loaded manifest's `auth.env_var` (otherwise 404). It resolves through the declarative compiler's existing `_default_env_resolver` under `workspace_scope(ws)`. **Known bug, fix in this task's first commit:** that resolver imports `get_setting` from `apps.api.database`, which does not exist. The swallowed `ImportError` means declarative providers only read `os.environ`, and keys saved in the settings DB are ignored. Fix the import to the real settings reader (the `_read_setting` pattern in `apps/api/services/leadgen/llm.py`), add `test_declarative_reads_settings_db`, and only then build the endpoint on top. The endpoint returns `{"value": "..."}` with `Cache-Control: no-store`, and never logs the value. It is not exposed through nginx.
- Go: `credentials.Client.Get(ctx, workspaceID, envVar string) (string, error)` — an in-memory cache keyed by `(workspaceID, envVar)` with a 60s TTL. Errors never include the value.

- [ ] **Step 1: Failing tests**: one eligibility test per rule (a passing workbook, then flip each condition → the expected reason); `test_payload_contains_no_secrets` (set `LEADMAGIC_API_KEY=sk-test-123`, enqueue → `"sk-test-123"` absent from `json.dumps(job.payload)`); `test_ineligible_stays_python`; credentials: `test_rejects_missing_token`, `test_rejects_wrong_token`, `test_rejects_non_manifest_var` (e.g. `DATABASE_URL` → 404), `test_value_not_logged` (caplog), `test_no_store_header`. Go: `TestCacheIsPerWorkspace`, `TestCacheExpires`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS, plus the full non-live suite.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go-executor): eligibility, payload v1, internal credential endpoint"`

### Task 12: Go `run_workbook` executor — cells, overlay, progress, receipts, telemetry

**Files:**
- Create: `internal/workbook/{run,cell,overlay,receipt,stats}.go` + `_test.go`, `internal/progress/redis.go`, `internal/db/queries/workbook.sql`
- Modify: `cmd/worker/main.go` (dispatch `run_workbook` → `workbook.Run`)

**Interfaces:**
- Consumes: `queue.Claim/Heartbeat/IsCancelled/Finalize`, `tenancy.InWorkspace`, `spend.*`, `manifest.Execute`, `egress.NewClient`, `credentials.Client`.
- `workbook.Run(ctx, deps Deps, c *queue.Claim) (Result, error)` with `Result{Completed, Errors, Total, Rows int; Stopped bool}`. This is the same shape `persist_run_result` stores.
- Behaviour to port, line by line, from `apps/api/services/workbook/enrichment.py`:
  - `_run_workbook_enrichment_impl`: loading, the `running` status, `topo_sort_columns` order (only enrichment columns are present, so this is stable input order), `fill_missing`, `force`, `row_ids/lead_ids/row_columns`, `retry_passes`, the pause/cancel check every ~2s, and finalization to `complete|failed|paused` with `completed_rows`.
  - The waterfall branch of `enrich_cell`, lines ~330–600: `row_identity = "row:<id>"|"lead:<id>"`; `chain_exposure`; reorder by prior attempts in this run; reserved calls for paid providers (`is_paid` ⇔ `costs[name] > 0`) via `spend.Reserve → Transition(dispatch) → manifest.Execute → AccountingEnvelope → Settle`; `provider_price_unknown`; stop at first success; timeout → `timeout`, or `accounting_uncertain` for reserved calls; scalar enforcement of the target value.
  - `_set_enrichment`: upsert `workbook_enrichments` by `(workbook_id, lead_id, column_id)` with `workspace_id`, and mirror into `workbook_rows.enrichments[col_id] = {"value","status","provider","error"}` (plus `skipped_providers` when present). Each cell commits in **one** `InWorkspace` transaction that first re-checks the job lease (`fence_workbook_run_state`). No network call happens inside a transaction.
  - Telemetry: port `planner.record_attempt` into `stats.go`, written in a separate short transaction after the cell commit, with failures logged and swallowed (as in Python).
  - Progress: `PUBLISH workbook:<workbook_id>` with the exact JSON key sets of the `running` / result `cell_update` and `workbook_status` messages in `enrich_cell` / `_run_workbook_enrichment_impl`.
  - Receipt: port `run_receipts.persist_run_result`, called after `Run` returns and before `Finalize`. Its failure is logged and does not cause a retry (matching Python).
- Cancellation: `ctx` is cancelled when `Heartbeat` reports lease loss or `IsCancelled` is true. No new `Reserve`/`dispatch` starts after that, and in-flight paid calls end `uncertain`.

- [ ] **Step 1: Failing integration tests** (providersim via httptest, PG under FORCE RLS): `TestRunHappyPath` (3 rows × 1 paid column → 3 complete cells, 3 settled attempts, `budget_spent_usd` += 0.15, overlay has 3 entries, the Redis subscriber sees 3 `running` + 3 result + 1 `workbook_status`); `TestWaterfallStopsAtFirstSuccess`; `TestNoMatchIsError_no_data`; `TestBudgetExhaustionStopsPaidCalls`; `TestFillMissingSkipsComplete`; `TestPauseStopsPromptly` (set the workbook `paused` mid-run → status `paused`, fewer than all cells attempted); `TestCancelMidWaterfall` (Review Focus 4: commit `cancelled` while the simulator holds call #2 → no call #3, job stays `cancelled`, the in-flight attempt is `uncertain`); `TestLeaseLossAbortsCommits` (steal the lease → no further cell writes); `TestCrashThenPythonReapsThenGoResumes` (SIGKILL mid-run → reaper → reclaim → settled attempts reused, no double charge); `TestTenantIsolationTwoWorkspacesConcurrent`.
- [ ] **Step 2: Run** `go test -race -tags integration ./internal/workbook/...` → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `git commit -s -m "feat(go): run_workbook executor for declarative waterfall columns"`

### Task 13: Parity harness, Compose service, rollout/rollback runbook, benchmark

**Files:**
- Create: `tests/test_go_parity.py` (markers `postgres`; skips if `go` is not on PATH), `tests/fixtures/go_parity/*.json`
- Create: `docs/go-executor.md`, `apps/backend-go/Dockerfile`
- Modify: `docker-compose.yml` (`worker-go` service, profile `go`, same network, no published ports, `depends_on: migrate (completed_successfully)`), `.github/workflows/release.yml` (build and push the Go image, record its digest in the release manifest), `pyproject.toml` (no new marker needed — reuse `postgres`)
- Modify: `apps/api/cli.py` — add `jobs reassign-executor --from go --to python [--workspace W]`, which updates only `status='pending'` rows

**Interfaces:**
- Parity test: for each fixture (happy, waterfall-fallback, no-match, 429, timeout, malformed, budget-cap, fill-missing, two-tenant) it runs the same workbook once with `executor='python'` and once with `executor='go'` against providersim with the same `SIM_SEED`. It then compares normalized snapshots of `workbook_enrichments`, `workbook_rows.enrichments`, `workbook_spend_attempts` (including `contract_hash`), `workbooks.{status,completed_rows,budget_spent_usd}`, the run receipt, and the captured Redis messages. Timestamps, UUIDs, `duration_ms` and `latency_ms` are removed before comparing.

- [ ] **Step 1: Write** `tests/test_go_parity.py` and fixtures. **Step 2: Run** `TEST_DATABASE_URL=... uv run pytest -q -m postgres tests/test_go_parity.py` → any diff is a bug in Go (fix it in the owning task's package), never a reason to loosen the normalizer without a written reason in the fixture.
- [ ] **Step 3: Rollback drill (documented in `docs/go-executor.md` and run once on staging):** enable the cohort → start a 10k-row run → turn the flag off → `docker compose stop worker-go` → `cli jobs reassign-executor --from go --to python` → Python reaps the processing claim and finishes the run. Expected: no duplicate settled attempts (`SELECT attempt_key, count(*) ... HAVING count(*)>1` → 0 rows) and every cell terminal.
- [ ] **Step 4: Benchmark:** rerun Task 4's matrix with the Go worker for the eligible workload, at identical hardware, limits, simulator profile and durability. Append a "Go vs Python" section to the baseline report. Record any target that is unsustainable and the rate actually achieved.
- [ ] **Step 5: Release gate (human):** parity green, rollback drill passed, no tenant leaks, benchmark published. Then enable for one internal workspace in production for 7 days before widening the cohort.
- [ ] **Step 6: Commit** `git commit -s -m "feat(go-executor): parity harness, compose service, rollout runbook, benchmarks"`

---

## Roadmap after M2 (each gets its own plan when started)

| Milestone | Next plan's first decision |
|---|---|
| M3 execution improvements | Shared Redis token-bucket per (provider, credential) across replicas; batched cell commits; Go-side provenance + automations outbox to widen eligibility |
| M4 Python specialists | gRPC + Buf contracts in `packages/contracts`; email-verify cascade and AI columns as specialist tasks with process pools |
| M5 API migration | First route domain to move (read-only workbook/rows endpoints), generated OpenAPI clients, FastAPI fallback |
| M6 multi-host | Move `data/workspaces.db` metadata + secrets and per-workspace collection ledgers to PostgreSQL; scheduler leader election |
| Optional Rust | Only with a profile from M0/M3 showing a CPU-bound kernel worth the RPC + build cost |
