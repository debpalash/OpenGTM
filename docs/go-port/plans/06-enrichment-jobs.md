# Enrichment Jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the workbook enrichment surface to Go: the 15 retained workbook routes (`run`, `run/estimate`, `stop`, cell run, `sources` add/run, `refresh`, `refresh-policy`, `migrate`, `meta/provider-stats`, workbook WebSocket) plus the 2 `ingest` routes and `GET /api/signals/sources`, platform-credit debit, and the full `run_workbook` / `refresh_workbook` / `signal_scan` job handlers with ~20 HTTP-only providers, the email-verify cascade and a provider cache, so that the Go worker runs every workbook column type and Python only serves what has no Go equivalent through one RPC.

**Architecture:** The Go worker (`cmd/worker`, `internal/workbook`) already runs `run_workbook` for declarative `enrichment`/`waterfall` columns and hands anything else back to Python (`queue.HandBack`). This plan (a) lifts those restrictions by porting column dependency ordering, formula/http columns and condition evaluation, (b) ports the HTTP providers behind one `providers.Provider` interface and routes every non-ported provider (LLM, browser, jobspy, staffspy, ddgs, libraries) through a single `pyworker` HTTP `ProviderCall` RPC to the Python API container with a hard per-call timeout, (c) ports `refresh_workbook` and `signal_scan`, and (d) ports the routes, including `check_and_debit`, in one transaction with the enqueue. Cell and run state, receipts, spend attempts and Redis events keep the byte formats the existing worker already writes.

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, go-redis v9, `golang.org/x/net/html` only if a provider needs it (pinned, Task 14), `github.com/coder/websocket` (decision D7, new pinned module); existing `internal/{httpx,identity,tenancy,audit,contract,queue,canonjson,manifest,spend,credentials,egress,billing,progress,ratelimit,workbook,wbcrud,signals}`. Python/uv for recorder, seed, vectors, Alembic and the Python-side RPC endpoint.

**Spec:** `docs/go-port/inventory.md` section 8 row 5 "Enrichment jobs" (sections 2 HTTP surface, 3 durable job types, 5 data stores, 6 background processes, 7 integrations, 9 risks); `docs/superpowers/plans/2026-10-26-go-first-worker.md` (the existing Go worker); `docs/go-port/plans/04-workbook-crud.md` (retained route list, conventions) and `05-leads-signals.md` (D1 ledger, `queue.Enqueue`, `signalsroutes`); GitHub issue #33. Python sources: `apps/api/routers/{workbooks,ingest,meta,signals}.py`, `services/workbook/{enrichment,refresh,provider_runner,vendor_catalog,column_deps,conditions,formula_column,http_column,cell_scope,source_engine,ingest}.py`, `services/go_executor.py`, `services/billing/service.py`, `services/leadgen/enrichment/{provider,cache,email_verify*,email_waterfall,email_deliverability}.py`, `.../providers/*.py`, `.../declarative/{manifest,signing}.py`, `services/signals/monitor.py`, `worker.py`, `scheduler.py`.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`, `migrations/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 02-05 merged (identity, tenancy, `wbcrud`, `leadsread`, `queue.Enqueue`, `signals`, `ratelimit`, Redis in `Deps`); the Go worker from the first-worker plan is in place (`internal/workbook`, `queue.HandBack`, `config.RequiredGoSchemaRevision`).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL: Alembic in `legacy/python/migrations` owns every schema change (Tasks 2, 4). New modules: only `github.com/coder/websocket` (D7), pinned.
- Tenant tables only inside `tenancy.InWorkspace` (API and worker). Runtime role non-superuser `NOBYPASSRLS`. Cross-workspace or unknown ids answer 404, never 403. Provider credentials are never shared across workspaces and never appear in payloads, logs, fixtures, receipts or error strings; the provider cache is workspace-scoped (D5).
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, auth gates, rate limits. Gates verbatim: run/stop/cell-run/sources/refresh/refresh-policy/migrate/ingest-token = `require_editor` (`identity.RequireRole("tables.write","editor","admin")`); estimate = `current_workspace`; `meta/provider-stats` = unauthenticated apart from the global auth middleware, exactly like Python (no `current_workspace`: it reads the global `provider_stats` table; verified in Task 1); `POST /rows/ingest` = session with `tables.write` or per-workbook ingest token (401 `Missing credentials`); WebSocket = `?token=` + `?workspace_id=` membership, close code 4403. Rate limits copied: `run` 20/min, `sources/{col}/run` 20/min, cell run 120/min (`internal/ratelimit`, slowapi key function reproduced).
- Behaviour parity of worker output is the release bar (cell values/statuses/errors, row overlay JSON, spend attempts and contract hashes, receipts, Redis event payloads); the existing `internal/workbook` parity tests keep passing unchanged. Python-generated vectors for every algorithm (cost, formula, deps, conditions, cache keys, cascade, intervals, signal ids, signing); provider behaviour pinned by Python-recorded HTTP transcripts (request and result), never hand-written.
- Security and crash hardening deviates from Python only as a documented deviation (decisions D2-D5, D7): SSRF-guarded egress on every provider, `http` column and SMTP/DNS probe (`internal/egress`, no redirects, dial-time IP check); bounded bodies (1 MiB), goroutines (`concurrency` from payload), WebSocket message size; debit and enqueue in one transaction.
- Scrapers stay a Python worker behind the job queue: `source_workbook`, `collect`, `bulk_enrich` keep `executor='python'`. Every provider that imports browser/jobspy/staffspy/ddgs/LLM/other non-HTTP libraries is listed in `internal/providers/python_providers.txt` and is called through `pyworker`. A test fails if a provider in `legacy/python/.../providers/*.py` is neither ported nor listed.
- `check_and_debit` is a no-op unless `BILLING_ENABLED` (Go `config.BillingEnabled`); with billing on, a run is debited once per `run_id` (`idempotency_key = "run:<run_id>"`), 4-decimal rounding, 402 `{"detail":{"message","balance_usd","required_usd"}}`.
- Redis in tests: `TEST_REDIS_URL` (127.0.0.1:56380). Integration tests skip when unset. HTTP providers are tested against `cmd/providersim` or recorded transcripts, never the internet.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (documented in `testdata/contract/retained_enrichment_routes.txt`, Task 32): `POST /api/v2/workbooks/{id}/generate-column` (LLM, D2), `GET /meta/provider-accuracy` (offline eval harness, D3), `GET /sources/{col}/preview` (runs a collection preview, D6), `/api/collect`, `/api/jobs*`, `POST /api/lead/{id}/enrich`, `/api/v2/search/unified`, `/api/v2/scraper/scrape`, `/leads/{verify-email,score,tech-stack,domain-intel}` (plan 05 D1 ledger and plan 13 scrapers).

## Review Focus

1. **Double or lost platform charge.** `run` debits then enqueues; Python commits the debit in its own transaction, so an enqueue failure loses credits and a retry with a new `run_id` double-charges. Go debits and enqueues in one transaction; same `run_id` twice (concurrent) charges once and the second caller gets the replay result; balance below cost is 402 with the exact detail and writes nothing; `projected <= 0`, billing off, all-BYOK runs never touch the ledger; `round(cost, 4)` and `round(balance - cost, 6)` match Python. Tasks 6, 24, 31.
2. **Two executors, one run.** A run can have columns Go executes and columns that go through the Python RPC, and `/stop` or a lease loss can land at any point: after the API commits `cancelled` no further provider call starts, `cancelled` is never overwritten by `completed`, a dispatched paid call stays `uncertain`, a Python RPC call is cancelled by context with a server-side kill, and a Go and a Python worker never both run the same job (executor routing and `HandBack` fencing from the first-worker plan). Cell run (in-request) racing a queued run on the same cell must not double-spend (spend attempt key). Tasks 12, 20, 21, 25.
3. **SSRF and credential tenancy in new egress.** The `http` column takes a user URL template, providers take user domains, the email cascade opens SMTP/DNS to attacker-chosen hosts: loopback, link-local, private, IPv4-mapped IPv6, DNS rebinding at dial time and redirects must be refused by `internal/egress`; a header or URL template must not leak another column's secret; workspace A's provider key and cache entries must never serve workspace B. Tasks 10, 11, 13-15.
4. **Formula, condition and template parity.** Python truthiness (`0`, `""`, `"N/A"`, `"nan"`), `str(float)` repr, integer vs float division, unicode `.lower()`, missing-column and circular references, a formula that Python rejects must be rejected (never widened), and an unbounded template/loop must be bounded. Tasks 9, 10.
5. **WebSocket and stream lifecycle.** Forged `workspace_id`, expired token, workbook of another tenant, and a missing workbook close with 4403 before accept; Redis down degrades to echo/pong mode; client disconnect releases the subscription and goroutine (reconnect storm), oversized frames close the socket, only `workbook:<id>` payloads for the authorised workbook are relayed byte-for-byte. Tasks 29, 32.
6. **Scan idempotency.** `signal_scan` ids come from `scan_signal_id(workspace, lead, type, natural_key)` with a period bucket, so a Go and a Python scan racing in the same period insert each signal once; one failing workspace or provider never aborts the others; the workbook trigger pass never enumerates workbooks outside a tenant scope. Task 17.

## File Structure

```
internal/billing/debit.go                                   # CheckAndDebit, ProjectedPlatformCost, InsufficientCreditsError
internal/vendorcatalog/{catalog,estimate}.go                # base_cost, is_paid, estimate_run_cost, costs table
internal/manifest/{registry,signing}.go                     # manifest index + Ed25519 verification (adds to existing package)
internal/colgraph/{deps,cycles,conditions}.go               # column_deps + conditions
internal/colcompute/{formula,httpcol}.go                    # formula_column + http_column
internal/enrichcache/{key,store}.go                         # workspace-scoped provider + email-verify cache
internal/pyworker/{client,types}.go                         # ProviderCall RPC client to Python
internal/providers/{provider,registry}.go, python_providers.txt
internal/providers/contact/*.go                             # hunter, apollo, prospeo, snov, pdl, debounce, abstractapi, numverify, reacher
internal/providers/firmo/*.go                               # ipinfo, gleif, companieshouse, mca, wikidata, secedgar, jsonld, techstack, atshiring, googlemaps
internal/emailverify/{cascade,waterfall,deliverability,smtp}.go
internal/providerrun/run.go                                 # Go-or-Python dispatch, timeout, stats ledger
internal/refresh/{policy,stale,job}.go
internal/signalscan/{scan,ids,sources,boost}.go
internal/runplan/{plan,eligibility,payload}.go              # run request -> executor, payload, projected cost
internal/workbook/{run,cell,columns}.go                     # (modify, Tasks 20-22)
cmd/worker/main.go                                          # (modify, Task 22: refresh_workbook, signal_scan handlers)
internal/api/{workbookestimate,workbookrun,workbookcell,workbooksources,workbookrefresh,workbookmigrate,workbookws,ingestroutes}.go
internal/api/register.go                                    # (modify once, Task 5)
internal/wbcrud/recompute.go                                # (modify, Task 31: billing-enabled recompute)
internal/db/queries/{billing_debit,enrich_cache,refresh,signalscan,runplan,ingest,provider_stats}.sql (+ gen/)
internal/db/schema.sql                                      # (regenerated, Task 4)
testdata/contract/{run,cellrun,sources,refresh,estimate,migrate,meta,ingest,ws}/*.json
testdata/contract/retained_enrichment_routes.txt
testdata/providers/<provider>/*.json                        # Python-recorded HTTP transcripts
../../legacy/python/migrations/versions/<rev>_enrichment_cache.py
../../legacy/python/apps/api/routers/internal_provider.py   # POST /internal/provider-call
../../legacy/python/scripts/{contract_seed.py,contract_cases/enrichment.yaml,gen_enrichment_vectors.py,record_provider_transcripts.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/enrichment-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time; larger waves run as batches). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors, transcripts; 2 Python Alembic cache tables; 3 Python RPC endpoint + executor routing; 4 Go schema sync; 5 Go scaffold | T1 owns `contract_seed.py`, `gen_enrichment_vectors.py`, `record_provider_transcripts.py`; T2 owns migrations; T3 owns `routers/internal_provider.py`, `go_executor.py`, `refresh.py` enqueue sites, `main.py` include line; T4 owns `schema.sql` and the revision constant; T5 owns `register.go`, `Deps` additions, all stub route files, `queue.EnqueueOnce` |
| 1 | 6 billing debit, 7 vendorcatalog, 8 manifest registry+signing, 9 colgraph, 10 colcompute, 11 enrichcache | 6 disjoint packages, each with its own `.sql` (6, 11); 8 only adds files to `internal/manifest/` |
| 2 | 12 pyworker, 13 contact providers, 14 firmographic providers, 15 emailverify, 16 refresh, 17 signalscan | 6 disjoint packages; 13 and 14 own `internal/providers/{contact,firmo}/` and each appends to its own registry file only (`contact/register.go`, `firmo/register.go`); 17 also owns stub `internal/api/signalsources.go` |
| 3 | 18 providerrun, 19 runplan | 2 new packages; 18 consumes 11-15, 19 consumes 6-8 and 9 |
| 4 (sequential) | 20 worker dependency ordering, 21 worker new column types, 22 worker refresh + signal_scan handlers | all three edit `internal/workbook/{run,cell,columns}.go` and `cmd/worker/main.go`; strictly one after another |
| 5 | 23 estimate/stop/provider-stats, 24 run, 25 cell run, 26 sources, 27 refresh routes, 28 migrate, 29 websocket, 30 ingest | each owns exactly one stub `internal/api/*.go` and one fixture dir; 8 tasks, run as batches of 4 + 4; 24 consumes 6, 19; 25 consumes 18, 21 |
| 6 | 31 billing recompute, 32 coverage + nginx + runbook | 31 edits plan 04's `wbcrud/recompute.go` and its test only; 32 consumes everything |

---

### Task 1: Python seed, recorder cases, vectors, provider transcripts

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/enrichment.yaml`, `legacy/python/scripts/gen_enrichment_vectors.py`, `legacy/python/scripts/record_provider_transcripts.py`
- Create (output, committed): `testdata/contract/{run,cellrun,sources,refresh,estimate,migrate,meta,ingest,ws}/*.json`, `internal/{vendorcatalog,billing,colgraph,colcompute,enrichcache,emailverify,refresh,signalscan,manifest,providers}/testdata/*.json`, `testdata/providers/<provider>/*.json`

**Interfaces:**
- Consumes: plan 04 seed (workspaces A/B, editor/viewer, workbooks `wb-alpha-1..2`, `wb-beta-1`).
- Produces: seed additions (workspace A with `BILLING_ENABLED` on for a second recording profile and a credit balance of 10.0; a workbook with enrichment, waterfall, ai_formula, formula, http, output, research and agent columns plus a circular pair; a v1 workbook with a leads filter; an ingest token; `provider_stats` rows; one `refresh_policy`; signals fixtures for scan) and fixtures the Tasks 6-32 assert against. Vector files: `estimate_run_cost`/`projected_platform_cost`/`check_and_debit` sequences (replay, insufficient, `0`, `-1`, `1e-5`), 60 formula cases (types, truthiness, division, string funcs, rejected syntax), 30 `cycle_blocked_columns`/ordering cases, 40 condition cases, cache `canonical_key` cases (accents, missing org, person-level), email cascade/waterfall/deliverability cases with scripted DNS/SMTP answers, `_interval_minutes`/`_stale_row_columns` cases, `scan_signal_id` cases (bucket boundary), manifest canonicalisation + signature verification (trusted, untrusted, invalid, tampered). Transcripts: for every ported provider, 6+ `{input, http_exchanges[], result}` recordings (hit, miss, 401, 429, 5xx, malformed JSON) captured by running the Python provider against a recording `httpx` transport.

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps; billing profile selected by env `BILLING_ENABLED=true` for the recorder).
- [ ] **Step 2: Write the cases** (each `{name, method, path, as, profile, expect_status}`): run happy path, `skipped` (no columns), `422 Cell scope requires matching explicit row IDs`, 422 circular columns, 402 under billing, 404 foreign workbook, viewer 403, cell run (value, `skipped`, `force`), sources add (`icp`, `people_search` 403 when disabled), sources run enqueue, refresh `{"status":"refreshing"}`, refresh-policy 200 and 404, estimate with `view_id`/`search`, migrate (fresh and already migrated), `meta/provider-stats`, ingest-token create/rotate, ingest with session, with token, without credentials 401, 413 too many rows, WS handshake cases as `{token, workspace_id, expect_close}`.
- [ ] **Step 3: Write the vector and transcript scripts**; they import the real Python modules.
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/enrichment.yaml`, `uv run python scripts/gen_enrichment_vectors.py`, `uv run python scripts/record_provider_transcripts.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 5: Commit** (Go/doc side only) `test(go): enrichment contract fixtures, vectors and provider transcripts (RED)`.

### Task 2: Python Alembic: enrichment cache tables

**Files:**
- Create: `legacy/python/migrations/versions/<rev>_enrichment_cache.py`; modify `legacy/python/apps/api/services/go_executor.py` (`REQUIRED_GO_SCHEMA_REVISION` set to the new head in Task 4)
- Test: `legacy/python/tests/test_migration_enrichment_cache.py`

**Interfaces:**
- Produces: tables `enrichment_cache(workspace_id TEXT NOT NULL, cache_key TEXT NOT NULL, field_name TEXT NOT NULL, value JSONB, provider TEXT, confidence DOUBLE PRECISION, cached_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL, PRIMARY KEY (workspace_id, cache_key, field_name))` and `email_verify_cache(workspace_id TEXT NOT NULL, email_hash TEXT NOT NULL, result JSONB NOT NULL, cached_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL, PRIMARY KEY (workspace_id, email_hash))`, both with ENABLE + FORCE RLS on `workspace_id = current_setting('app.workspace_id', true)` and grants to the runtime role (follow `b3c4d5e6f7a8` for the grant pattern). Columns mirror `enrichment/cache.py` and `email_verify_cache.py`; the migration is Python-owned and has a working downgrade.

- [ ] **Step 1: Write the failing migration test** (`upgrade`, RLS blocks cross-workspace select with a non-superuser role, `downgrade` drops both). **Step 2: Run** `uv run pytest tests/test_migration_enrichment_cache.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS and `alembic heads` shows one head.
- [ ] **Step 5: Commit** (Python side is unversioned: note the revision id in the PR description; nothing to `git add`).

### Task 3: Python RPC endpoint and executor routing

**Files:**
- Create: `legacy/python/apps/api/routers/internal_provider.py`; modify `legacy/python/apps/api/main.py` (include line), `legacy/python/apps/api/services/go_executor.py`, `legacy/python/apps/api/services/workbook/refresh.py` (`_enqueue_next`, `bootstrap_signal_scan`, `handle_signal_scan` enqueue sites)
- Test: `legacy/python/tests/test_internal_provider.py`, `legacy/python/tests/test_executor_routing.py`

**Interfaces:**
- Produces: `POST /internal/provider-call` body `{"call_id": str, "provider": str, "workspace_id": str, "input": object, "timeout_s": float}` -> `200 {"status": "ok"|"error"|"timeout", "output": object|null, "error": str|null, "cost_usd": float}`; bearer token = the existing `INTERNAL_CREDENTIALS_TOKEN` mechanism of `internal_credentials.py` (401 without, constant-time compare); runs the provider in `provider_runner` (pebble kill on `timeout_s`, hard cap 120); resolves the workspace's credentials itself (the secret never crosses the wire); `call_id` deduplicates a retried call; `provider` must be in `python_providers.txt` or any registered provider (404 `unknown provider`). Executor routing: `refresh_workbook` and `signal_scan` enqueue with `executor='go'` when `GO_EXECUTOR_ENABLED` and the workspace is in `GO_EXECUTOR_WORKSPACES` (signal_scan is global: follows `GO_EXECUTOR_SIGNAL_SCAN`, default false); `go_executor.py` `GO_COLUMN_TYPES` is extended in Task 21's rollout flag, not here.

- [ ] **Step 1: Write failing tests**: `test_provider_call_requires_token`, `test_provider_call_timeout_kills_worker`, `test_provider_call_dedupes_call_id`, `test_provider_call_never_returns_secret`, `test_refresh_enqueue_executor_follows_flag`, `test_signal_scan_global_flag`.
- [ ] **Step 2: Run** `uv run pytest tests/test_internal_provider.py tests/test_executor_routing.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 4: Go schema sync

**Files:**
- Modify: `internal/db/schema.sql` (via `make schema` against a migrated database), `internal/config/config.go` (`RequiredGoSchemaRevision`), `internal/db/queries/enrich_cache.sql` (create, empty header only)
- Test: `internal/db/schema_test.go` (existing drift test)

**Interfaces:** Consumes the Task 2 revision id. Produces `enrichment_cache`, `email_verify_cache` in `schema.sql` and the bumped revision (the worker refuses to start below it).

- [ ] **Step 1: Write failing test** `TestSchemaHasEnrichmentCacheTables` (tables, PK, RLS flags). **Step 2: Run** `make test` -> FAIL. **Step 3:** run `make schema`, bump `RequiredGoSchemaRevision`. **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): enrichment cache schema (RED)`, `feat(go): sync enrichment cache schema and required revision`.

### Task 5: Scaffold: Deps, register lines, stubs, EnqueueOnce

**Files:**
- Modify: `internal/api/register.go` (+`RegisterWorkbookRuns`, `RegisterIngest`, `RegisterWorkbookWS` lines, `Deps` gains `PyWorker *pyworker.Client`, `Limiter` if absent), `internal/config/config.go` (`PY_WORKER_URL`, `PY_WORKER_TOKEN`, `GO_RUN_ROUTES_ENABLED` default false, `WS_MAX_MESSAGE_BYTES` default 65536)
- Create: stub files `internal/api/{workbookestimate,workbookrun,workbookcell,workbooksources,workbookrefresh,workbookmigrate,workbookws,ingestroutes,signalsources}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `internal/pyworker/types.go` (types only), `internal/queue/enqueueonce.go`, `internal/db/queries/runplan.sql`
- Test: `internal/api/enrichscaffold_test.go`, `internal/queue/enqueueonce_integration_test.go`

**Interfaces:**
- Produces: `RegisterWorkbookRuns/RegisterIngest/RegisterWorkbookWS(r *httpx.Router, d Deps)` (register nothing and log when `PG_LEAD_STORE` or `GO_RUN_ROUTES_ENABLED` is false); `func EnqueueOnce(ctx context.Context, tx pgx.Tx, jobType, workspaceID string, payload any, fireKey, executor string) (id int64, created bool, err error)` mirroring `enqueue_job_once` (`pg_advisory_xact_lock` on the fire key hash, no second pending/running row for the same key, `executor` explicit); `queue.Enqueue` from plan 05 gains no change. `pyworker.ProviderCall{CallID, Provider, WorkspaceID string; Input json.RawMessage; Timeout time.Duration}` and `ProviderResult{Status string; Output json.RawMessage; Error string; CostUSD float64}`.

- [ ] **Step 1: Write failing tests**: `TestEnrichRoutesNotRegisteredWithoutFlags`, `TestEnrichRoutePatterns` (exactly the routes of Tasks 23-30 and `signals/sources`), `TestEnqueueOnceDedupesConcurrent` (16 goroutines, one row), `TestEnqueueOnceRowShapeMatchesPython`, `TestEnqueueOnceRollsBackWithTx`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): enrichment scaffold and enqueue-once (RED)`, `feat(go): enrichment route scaffold and enqueue-once helper`.

### Task 6: Platform credit debit

**Files:**
- Create: `internal/db/queries/billing_debit.sql`, `internal/billing/debit.go`, `internal/billing/debit_integration_test.go`

**Interfaces:**
- Consumes: existing `billing.DebitResult`, `Balance`, `Credit` (`internal/billing/ledger.go`).
- Produces: `type InsufficientCreditsError struct{ BalanceUSD, RequiredUSD float64 }` (`Error()` text equals Python `InsufficientCreditsError.__str__`); `func CheckAndDebit(ctx context.Context, tx pgx.Tx, enabled bool, workspaceID string, projectedUSD float64, runID, reason string) (DebitResult, error)` implementing `service.py:176-236` (disabled or `cost <= 0` after `round(x,4)` -> `charged 0`, balance; existing `run:<runID>` ledger entry -> `IdempotentReplay`; `SELECT ... FOR UPDATE` balance row, insufficient -> `*InsufficientCreditsError` with no write; else ledger entry `amount_usd = -cost`, balance `round(current - cost, 6)`; unique-violation on the idempotency key -> replay); `func ProjectedPlatformCost(numRows int, providersByCol map[string][]string, columnCounts map[string]int) float64` equal to `service.py:69-90`. It runs inside the caller's transaction (it does not commit).

- [ ] **Step 1: Write failing tests** against Task 1 vectors: `TestCheckAndDebitMatchesVectors`, `TestDebitDisabledIsNoop`, `TestDebitZeroAndNegativeAreNoops`, `TestDebitReplaySameRunIDChargesOnce`, `TestDebitConcurrentSameRunIDChargesOnce` (16 goroutines), `TestDebitInsufficientWritesNothing`, `TestDebitRollsBackWithCallerTx`, `TestDebitTenantIsolation`, `TestProjectedPlatformCostMatchesVectors`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): platform credit debit (RED)`, `feat(go): check-and-debit and projected platform cost`.

### Task 7: Vendor catalog and run estimate

**Files:**
- Create: `internal/vendorcatalog/{catalog,estimate}.go`, `internal/vendorcatalog/catalog_test.go`

**Interfaces:**
- Produces: `func BaseCost(name string) float64`, `IsPaid(name string) bool`, `HasKnownCost`, `IsBYOK(name string) bool` (reads `vendor_catalog.py` constants and the manifest-declared cost via `manifest.Index`), `func CalculateCost(name, operation string, params map[string]any) float64`, `func EstimateRunCost(numRows int, providersByCol map[string][]string) *Obj` (ordered object, key order and float repr equal to `estimate_run_cost`, `vendor_catalog.py:154-196`), `func ListVendors() []Obj`. The cost table is generated from Python by Task 1 into `testdata/vendor_costs.json` and loaded with `go:embed`; a test fails if a Python-catalog entry is missing.

- [ ] **Step 1: Write failing tests**: `TestCostTableMatchesPython`, `TestEstimateRunCostMatchesVectors` (research column worst case, empty waterfall, unknown provider, free providers), `TestIsBYOKFollowsEnvVar`, `TestEstimateFloatRepr`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): vendor catalog and run estimate (RED)`, `feat(go): vendor catalog costs and run cost estimate`.

### Task 8: Manifest registry and signature verification

**Files:**
- Create: `internal/manifest/{registry,signing}.go`, `internal/manifest/registry_test.go`, `internal/manifest/signing_test.go`

**Interfaces:**
- Consumes: existing `manifest.Manifest` types (`types.go`).
- Produces: `type SigStatus string` (`trusted`, `untrusted`, `invalid`, `unsigned`); `func Verify(path string, trust TrustStore) (SigStatus, error)` equal to `signing.verify_manifest`; `func CanonicalManifest(m Manifest) ([]byte, error)` byte-identical to `signing.canonical_manifest`; `type Entry struct{ Manifest Manifest; SHA256 string }`; `func LoadIndex(dir string, policy string, trust TrustStore) (map[string]Entry, error)` equal to `go_executor.load_manifest_index` (`invalid`/`untrusted` skipped, `required` policy needs `trusted`). The Go API now builds payload manifests itself (Task 19), so the hash must equal the Python one that the worker and spend contracts already use.

- [ ] **Step 1: Write failing tests** with Task 1 vectors: `TestCanonicalManifestMatchesPython`, `TestVerifyTrustedUntrustedInvalid`, `TestVerifyRejectsTamperedBody`, `TestLoadIndexPolicyRequired`, `TestLoadIndexSkipsInvalid`, `TestLoadIndexHashEqualsPython`, `TestManifestPathTraversalRejected` (symlink or `..` in `dir` walk).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** with `crypto/ed25519`; do not add modules. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): manifest registry and signing (RED)`, `feat(go): manifest index and signature verification`.

### Task 9: Column dependency graph and conditions

**Files:**
- Create: `internal/colgraph/{deps,cycles,conditions}.go`, tests

**Interfaces:**
- Produces: `type Column = map[string]any` ordered by `Keys []string` as in plan 05 `Rec` (`type Col struct{ Keys []string; Vals map[string]any }`); `func Refs(c Col) []string` (names inside `{...}` of `prompt`, `formula`, `http_url`, `goal`, plus `input_columns`), `func CycleBlocked(cols []Col) map[string]bool` (= `column_deps.cycle_blocked_columns`), `func Order(cols []Col) ([]Col, error)` (stable topological order equal to Python's run order), `func Upstream(cols []Col, id string) []string`, `func EvalCondition(cond any, row map[string]any) bool` (= `conditions.py`).

- [ ] **Step 1: Write failing tests** with vectors: `TestCycleBlockedMatchesVectors`, `TestOrderStableAndMatchesPython`, `TestRefsByColumnNameAndID`, `TestSelfReferenceIsCycle`, `TestConditionVectors` (40 cases incl. `0`, `""`, `"N/A"`, `None`), `TestConditionUnknownOperatorFalseLikePython`, `TestOrderBoundedOnLargeGraph` (2,000 columns under 1 s).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): column graph and conditions (RED)`, `feat(go): column dependency ordering and conditions`.

### Task 10: Formula and http columns

**Files:**
- Create: `internal/colcompute/{formula,httpcol}.go`, tests

**Interfaces:**
- Consumes: `internal/egress` client; `colgraph.Refs`.
- Produces: `func EvalFormula(expr string, row map[string]any) (any, error)` (a hand-written parser and evaluator for exactly the grammar of `formula_column.py`; unsupported syntax returns the same error string Python gives; values, `str(float)` repr and truthiness equal vectors; evaluation bounded to 10,000 nodes and 100 ms); `type HTTPColumn struct{ URL, Method string; Headers map[string]string; Body, JSONPath string; Timeout time.Duration }`; `func RunHTTPColumn(ctx context.Context, c *http.Client, col HTTPColumn, row map[string]any, secrets func(name string) (string, bool)) (HTTPResult, error)` equal to `http_column.py` (template expansion, response projection, error strings), no redirects, 1 MiB body cap, URL re-validated by the egress guard after template expansion.

- [ ] **Step 1: Write failing tests**: `TestFormulaVectors` (60 cases), `TestFormulaRejectsWhatPythonRejects`, `TestFormulaNodeBudget`, `TestHTTPColumnVectors`, `TestHTTPColumnBlocksPrivateAfterTemplate` (`{url}` resolving to `127.0.0.1`, `169.254.169.254`, `[::ffff:10.0.0.1]`), `TestHTTPColumnNoRedirects`, `TestHTTPColumnSecretNotInError`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): formula and http columns (RED)`, `feat(go): formula evaluator`, `feat(go): http column executor`.

### Task 11: Provider and email-verify cache

**Files:**
- Create: `internal/db/queries/enrich_cache.sql` (fill), `internal/enrichcache/{key,store}.go`, tests

**Interfaces:**
- Consumes: Task 4 tables; `tenancy.InWorkspace`.
- Produces: `func CanonicalKey(field string, lead Lead) (key string, ok bool)` (= `cache.canonical_key`, `ok=false` when identity is insufficient); `func Get(ctx, tx pgx.Tx, workspaceID, key, field string, now time.Time) (Hit, bool, error)`; `func Put(ctx, tx, workspaceID, key, field string, h Hit, ttl time.Duration) error`; `func GetEmail(ctx, tx, workspaceID, email string, now time.Time) (json.RawMessage, bool, error)`, `PutEmail(...)` (hash of the lower-cased email exactly like `email_verify_cache.py`); expired rows are never returned and are overwritten by `Put`.

- [ ] **Step 1: Write failing tests**: `TestCanonicalKeyMatchesVectors`, `TestCacheTTLExpiry`, `TestCacheWorkspaceIsolation` (A's entry invisible to B, pool of 1 alternating tenants), `TestCachePutOverwrites`, `TestEmailCacheKeyLowercasedHash`, `TestCacheValueRoundTripKeyOrder`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): enrichment cache (RED)`, `feat(go): workspace-scoped provider and email-verify cache`.

### Task 12: Python specialist RPC client

**Files:**
- Create: `internal/pyworker/client.go`, `internal/pyworker/client_test.go`, `internal/pyworker/client_integration_test.go`

**Interfaces:**
- Consumes: Task 5 types; Task 3 endpoint.
- Produces: `func New(baseURL, token string, hc *http.Client) *Client`; `func (c *Client) Call(ctx context.Context, call ProviderCall) (ProviderResult, error)`: POST `/internal/provider-call` with bearer token, `timeout_s` from `call.Timeout` (cap 120 s), client timeout `Timeout + 5 s`; context cancel aborts the request (Python side kills the child); 5xx and connection errors map to `ProviderResult{Status:"error"}` with a non-secret message after one retry with the same `CallID`; response body capped at 4 MiB; the token never appears in errors or logs; `ErrUnavailable` when `PY_WORKER_URL` is empty (callers hand the job back).

- [ ] **Step 1: Write failing tests** (httptest server): `TestCallSendsBearerAndCallID`, `TestCallTimeoutAndCancel`, `TestCallRetriesOnceWithSameCallID`, `TestCallBodyCap`, `TestCallErrorsNeverContainToken`, `TestUnavailableWhenNoURL`, and an integration `TestCallAgainstPythonEndpoint` (skips unless `TEST_PY_WORKER_URL`).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): python worker RPC client (RED)`, `feat(go): python provider-call RPC client`.

### Task 13: Contact and email HTTP providers

**Files:**
- Create: `internal/providers/{provider,registry}.go`, `internal/providers/contact/{hunter,apollo,prospeo,snov,pdl,debounce,abstractapi,numverify,reacher,register}.go`, `internal/providers/python_providers.txt` (owned here for the contact group; Task 14 appends its own section), tests, `testdata/providers/<name>/*.json` (Task 1)

**Interfaces:**
- Produces: `type Input struct{ Lead map[string]any; Field string; Operation string; Settings map[string]any }`, `type Output struct{ Value any; Confidence float64; Extra *Obj; CostUSD float64; Error string }`, `type Provider interface{ Name() string; Enrich(ctx context.Context, c *http.Client, key func() (string, error), in Input) (Output, error) }`, `func Register(p Provider)`, `func Get(name string) (Provider, bool)`. The key is resolved lazily through `credentials.Client.Get(ctx, workspaceID, envVar)` so a missing key is the same error string Python returns and nothing is fetched for free providers. Providers: the nine listed (each file is a port of the matching `providers/*.py`; `holehe_verify`, `mailscout_verify` import Python libraries and go to `python_providers.txt` with a reason).

- [ ] **Step 1: Write failing tests**: `TestProviderTranscripts` (table over `testdata/providers/<name>/*.json`: Go emits the same method, URL (query order), headers minus the key header, body, and returns the recorded result), `TestMissingKeyErrorMatchesPython`, `TestProviderHonoursEgressGuard` (a transcript whose host resolves private is refused), `TestProviderNoRedirects`, `TestProviderBodyCap`, `TestEveryProviderPortedOrListed` (scans `legacy/python/.../providers/*.py`).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** each provider in its own file (commit per two providers). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): contact provider transcripts (RED)`, `feat(go): hunter, apollo and prospeo providers`, `feat(go): snov, people data labs and debounce providers`, `feat(go): abstract, numverify and reacher providers`.

### Task 14: Firmographic and registry HTTP providers

**Files:**
- Create: `internal/providers/firmo/{ipinfo,gleif,companieshouse,mca,wikidata,secedgar,jsonld,techstack,atshiring,googlemaps,register}.go`, tests; append the firmo section to `internal/providers/python_providers.txt`

**Interfaces:**
- Consumes: Task 13 `providers.Provider`. Produces ten registered providers; `jsonld` and `techstack` parse HTML with `golang.org/x/net/html` (pinned, no other new module) and must equal Python extraction vectors, including malformed markup and a 1 MiB cap; `secedgar` honours the SEC `User-Agent` and 10 req/s limiter (`golang.org/x/time` is not added: use a ticker-based limiter in the file); everything else in the `providers/*.py` set that imports a non-HTTP library (company_intel, facebook_pages, local_business, email_harvester, crosslinked, ddg_*, jobspy_signals, social_finder, deep_scraper, staffspy, website_scraper, decision_maker, lead_scorer, job_tech_intent) is listed in `python_providers.txt` with its reason.

- [ ] **Step 1: Write failing tests**: `TestFirmoProviderTranscripts`, `TestJSONLDExtractionVectors`, `TestTechStackVectors`, `TestSECUserAgentAndRateLimit`, `TestHTMLBodyCap`, `TestEveryProviderPortedOrListed` (shared with Task 13, must now pass with both sections).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** (commit per three providers). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): firmographic provider transcripts (RED)`, `feat(go): ipinfo, gleif and wikidata providers`, `feat(go): companies house, mca and sec edgar providers`, `feat(go): jsonld, tech stack and ats providers`, `feat(go): google maps provider`.

### Task 15: Email verify cascade, waterfall, deliverability

**Files:**
- Create: `internal/emailverify/{cascade,waterfall,deliverability,smtp}.go`, tests

**Interfaces:**
- Consumes: `enrichcache.GetEmail/PutEmail`, `providers.Get`, `egress`.
- Produces: `type Result struct{ Status string; Provider string; Reason string; ... }` with the Python key order; `func Cascade(ctx, d Deps, workspaceID, email string) (Result, error)` (= `email_verify_cascade.py`, order of verifiers and short-circuits identical); `func Waterfall(ctx, d Deps, workspaceID string, lead map[string]any) (WaterfallResult, error)` (= `email_waterfall.py`); `func Deliverability(ctx, d Deps, domain string) (DeliverabilityResult, error)` (MX/SPF/DMARC via `net.Resolver`); `func ProbeSMTP(ctx, d Deps, host, email string) (Probe, error)` with dial-time egress guard (private MX hosts are refused), 10 s total budget, `RCPT TO` only, never sends mail. Verifiers that depend on Python libraries (`holehe`, `mailscout`) go through `pyworker`.

- [ ] **Step 1: Write failing tests** with scripted DNS and an in-process SMTP server: `TestCascadeVectors`, `TestWaterfallVectors`, `TestDeliverabilityVectors`, `TestSMTPProbeRefusesPrivateMX`, `TestSMTPProbeNeverSendsData`, `TestCascadeUsesCache`, `TestCascadeCacheIsWorkspaceScoped`, `TestCascadeHonoursContextCancel`, `TestCatchAllDetection`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): email verification (RED)`, `feat(go): email deliverability and smtp probe`, `feat(go): email verify cascade and waterfall`.

### Task 16: Refresh policy and stale-cell selection

**Files:**
- Create: `internal/db/queries/refresh.sql`, `internal/refresh/{policy,stale,job}.go`, tests

**Interfaces:**
- Consumes: `queue.EnqueueOnce`; `wbcrud` column/row types.
- Produces: `func IntervalMinutes(policy map[string]any) (int, bool)` (= `_interval_minutes`: `hourly`/`daily`/`weekly`/int minutes); `func StaleRowColumns(ctx, tx, wbID string, cols []Col, ttlDays map[string]float64, now time.Time) (map[int64][]string, error)` (= `_stale_row_columns`); `func SetPolicy(ctx, tx, wbID string, policy map[string]any) (map[string]any, error)` (= `set_refresh_policy`: `{"error": ...}` for unknown workbook, stored shape, next-run enqueue when `enabled`); `func Handle(ctx, d Deps, c *queue.Claim) error` (= `handle_refresh_workbook`: source new rows via `source_workbook` enqueue (Python executor), re-enrich stale columns by enqueueing `run_workbook`, log activity rows, enqueue next with `fire_key`).

- [ ] **Step 1: Write failing tests**: `TestIntervalMinutesVectors`, `TestStaleRowColumnsVectors`, `TestSetPolicyShapeAndNextEnqueue`, `TestRefreshEnqueuesRunForStaleOnly`, `TestRefreshNextIsOncePerFireKey`, `TestRefreshTenantIsolation`, `TestRefreshMissingWorkbookCompletesQuietly`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): refresh policy and stale selection (RED)`, `feat(go): refresh policy and stale cell selection`, `feat(go): refresh_workbook job handler`.

### Task 17: Signal scan and signal sources route

**Files:**
- Create: `internal/db/queries/signalscan.sql`, `internal/signalscan/{scan,ids,sources,boost}.go`, tests; modify stub `internal/api/signalsources.go` (created by Task 5)

**Interfaces:**
- Consumes: plan 05 `signals` package (reads); `automationhook` (`on_signal` emit); `pyworker` for the hiring detector (`jobspy_signals`); `queue.EnqueueOnce`.
- Produces: `func ScanSignalID(workspaceID string, leadID int64, signalType, naturalKey string, ts time.Time) string` (= `monitor.scan_signal_id` with `_period_bucket`); `func Run(ctx, d Deps) (Counts, error)` (= `run_signal_scan`: per workspace under `tenancy.InWorkspace`; SEC and news over HTTP; hiring through `pyworker`; `INSERT ... ON CONFLICT (id) DO NOTHING`; `_apply_signal_boosts`; `on_signal` emitted only for rows actually inserted; one workspace failing is logged and skipped); `func HandleScan(ctx, d Deps, c *queue.Claim) error` (= `handle_signal_scan`: global job, then per workspace trigger pass enqueuing `refresh_workbook` for workbooks whose `refresh_policy.on_signal` matches fired types); `GET /api/signals/sources` byte-identical to Python (auth as in `signals.py`).

- [ ] **Step 1: Write failing tests**: `TestScanSignalIDVectors`, `TestScanIsIdempotentAcrossRuns`, `TestScanRacingPythonInsertsOnce` (pre-insert the Python-id row), `TestOnSignalOnlyForNewRows`, `TestOneWorkspaceFailureDoesNotAbort`, `TestTriggerPassScopedPerWorkspace`, `TestBoostsMatchPython`, `TestSignalSourcesMatchesFixture`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): signal scan (RED)`, `feat(go): scan signal ids and inserts`, `feat(go): signal scan handler and workbook trigger pass`, `feat(go): signals sources route`.

### Task 18: Provider runner (Go or Python dispatch)

**Files:**
- Create: `internal/providerrun/run.go`, `internal/db/queries/provider_stats.sql`, tests

**Interfaces:**
- Consumes: Tasks 11-15 (`providers`, `enrichcache`), 12 (`pyworker`), 7 (`vendorcatalog`).
- Produces: `type Runner struct{ ... }`, `func New(d Deps) *Runner`, `func (r *Runner) Run(ctx context.Context, workspaceID, provider string, in providers.Input, timeout time.Duration) (providers.Output, error)`: Go-native provider in a goroutine under `context.WithTimeout` (hard cap 120 s; a provider that ignores ctx is abandoned and counted, bounded by a semaphore of `provider_workers`), otherwise `pyworker.Call` with the same timeout; `ErrUnavailable` is returned (not a cell error) so the worker can hand the job back; cache read-through for cacheable fields; per-call `provider_stats` upsert (`attempts`, `hits`, `latency`, `cost`) identical to Python's ledger writes and readable by Task 23. Concurrency of abandoned calls is visible via a counter in `telemetry`.

- [ ] **Step 1: Write failing tests**: `TestRunGoProvider`, `TestRunPythonProviderViaRPC`, `TestRunTimeoutDoesNotBlockSlot`, `TestAbandonedCallsBounded`, `TestRunCacheHitSkipsProvider`, `TestRunCancelPropagatesToRPC`, `TestStatsLedgerRowsMatchPython`, `TestUnavailableIsDistinctError`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): provider runner (RED)`, `feat(go): provider runner with Go and Python dispatch`.

### Task 19: Run planning and executor eligibility

**Files:**
- Create: `internal/runplan/{plan,eligibility,payload}.go`, tests; extend `internal/db/queries/runplan.sql`

**Interfaces:**
- Consumes: Tasks 6-9 (`billing.ProjectedPlatformCost`, `vendorcatalog`, `manifest.LoadIndex`, `colgraph`).
- Produces: `type RunRequest struct{ ColumnIDs []string; RowIDs, LeadIDs []int64; RowColumns map[string][]string; ViewID, Search string; ... }` (field validation messages equal Python); `type Plan struct{ Columns []Col; Rows []int64; RunID string; Executor string; Payload json.RawMessage; ProjectedUSD float64; Skipped bool }`; `func Build(ctx, tx pgx.Tx, wb Workbook, req RunRequest, cfg Config) (Plan, error)` equal to `workbooks.py:1346-1500` (enrichment-type selection, output columns last, `restrict_work_items`, cell-scope 422 strings, cycle 422, v2 row resolution with the exact row set resolved once, v1 fallback, `row_execution_data`); `func Eligible(cols []Col, idx map[string]manifest.Entry, cfg Config) (goOK bool, reason string)` equal to `go_executor.py` (extended in Task 21 as the rollout flag allows non-declarative types); payload byte layout equals the existing `workbook/payload.go` (golden test against a Python-built payload).

- [ ] **Step 1: Write failing tests**: `TestBuildMatchesRunFixtures`, `TestOutputColumnsRunLast`, `TestCellScopeErrors` (3 messages verbatim), `TestCycle422Message`, `TestSkippedWhenNoColumns`, `TestRowSetResolvedOnce` (view/search resolved in the planner, not the worker), `TestPayloadMatchesPythonGolden`, `TestEligibilityMatrix`, `TestExecutorGoOnlyInCohort`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): run planning (RED)`, `feat(go): run request resolution and cell scope`, `feat(go): executor eligibility and payload build`.

### Task 20: Worker: dependency ordering and upstream failure

**Files:**
- Modify: `internal/workbook/columns.go`, `internal/workbook/run.go` (remove the `columnRefs` hand-back; order with `colgraph.Order`; propagate upstream failure/blocked status exactly as `_run_one_row`), `internal/workbook/columns_test.go`, `internal/workbook/run_integration_test.go`

**Interfaces:** Consumes `colgraph.Order`, `CycleBlocked`. Produces: columns with `{col}` references now run in Go; cyclic columns end `blocked` with Python's error string; an upstream failure marks the dependent cell with Python's message without calling its provider.

- [ ] **Step 1: Write failing tests**: `TestRefColumnsNoLongerHandedBack`, `TestRunOrderFollowsDependencies`, `TestCyclicColumnBlockedMessage`, `TestUpstreamFailureSkipsDependent`, `TestRunStillHandsBackNonGoTypes` (until Task 21), existing parity tests unchanged.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): worker dependency ordering (RED)`, `feat(go): worker runs columns in dependency order`.

### Task 21: Worker: remaining column types and structured fields

**Files:**
- Modify: `internal/workbook/{cell,run,columns}.go`, `internal/runplan/eligibility.go` (`GO_COLUMN_TYPES` extended behind `GO_EXECUTOR_COLUMN_TYPES`), `cmd/worker/main.go` (builds `providerrun.Runner`, `pyworker.Client`); tests

**Interfaces:**
- Consumes: Tasks 10, 12, 18. Produces: `formula` and `http` columns execute in Go; `ai_formula`, `research`, `agent` and the AI batch prepass execute through `pyworker` (`provider: "ai_formula"` etc., input = row values + column config) with the Python-side result written by Go into cells, receipts and spend attempts in the existing formats; `output` columns stay on Python (hand-back, plan 09 destinations) and so does any job that contains one; structured write-back fields (`decision_makers`, `hiring_signals`, `secondary_emails`, `secondary_phones`, `technographics`) are applied to the lead via the plan 05 write store; `enrichment`/`waterfall` columns use `providerrun` for every provider (Go-native or Python). Cancellation: context cancel cancels the in-flight RPC; a dispatched paid call stays `uncertain`.

- [ ] **Step 1: Write failing tests**: `TestFormulaColumnCellsMatchPython`, `TestHTTPColumnCellsMatchPython`, `TestAIColumnViaPyworkerWritesCell`, `TestPyworkerUnavailableHandsBackJob`, `TestOutputColumnHandsBack`, `TestStructuredFieldsWriteBack`, `TestCancelMidRPCLeavesUncertain`, `TestNoOverwriteOfCancelled`, `TestSpendAttemptKeysMatchPythonForPyworkerCalls`, `TestRedisCellEventsMatchPython`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): worker column types (RED)`, `feat(go): worker formula and http columns`, `feat(go): worker ai, research and agent columns via python RPC`, `feat(go): worker structured field write-back`.

### Task 22: Worker: refresh and signal_scan handlers

**Files:**
- Modify: `cmd/worker/main.go` (`buildRunner` dispatch on `c.Type`: `run_workbook`, `refresh_workbook`, `signal_scan`; per-type timeouts 1800/900/300 s from `JOB_TIMEOUTS`), `cmd/worker/main_test.go`, `cmd/worker/main_integration_test.go`

**Interfaces:** Consumes `refresh.Handle` (Task 16), `signalscan.HandleScan` (Task 17). Produces: the Go worker claims `executor='go'` rows of these two types with the same lease, heartbeat, cancel, retry and timeout semantics as `run_workbook`; unknown types are never claimed.

- [ ] **Step 1: Write failing tests**: `TestRunnerDispatchesByType`, `TestPerTypeTimeouts`, `TestRefreshJobEndToEnd`, `TestSignalScanJobEndToEnd`, `TestUnknownTypeNotClaimed`, `TestPanicInHandlerFailsJobNotProcess`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): worker job dispatch (RED)`, `feat(go): worker handles refresh_workbook and signal_scan`.

### Task 23: Estimate, stop, provider-stats

**Files:**
- Modify: stub `internal/api/workbookestimate.go`; create `testdata/contract/{estimate,meta}/*.json` assertions in `internal/api/workbookestimate_test.go`

**Interfaces:** Consumes `vendorcatalog.EstimateRunCost`, `wbcrud` row query for `view_id`/`search`. Produces: `GET /{id}/run/estimate?view_id&search` (`search` max 500, 422 otherwise; result with `workbook_id` appended last), `POST /{id}/stop` (marks the workbook's pending/running `run_workbook` jobs `cancelled` and returns Python's body, read from `workbooks.py:1628`), `GET /api/workbooks/meta/provider-stats` -> `{"stats":[...]}` ordered by `attempts` desc, ties by Python's order.

- [ ] **Step 1: Write failing tests**: `TestEstimateMatchesFixtures`, `TestEstimateEmptyWaterfallDisablesProviders`, `TestEstimateSearchTooLong422`, `TestStopCancelsPendingAndRunning`, `TestStopForeignWorkbook404`, `TestStopViewerForbidden`, `TestProviderStatsMatchesFixture`, `TestProviderStatsTieOrder`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): estimate, stop and provider stats routes (RED)`, `feat(go): run estimate and provider stats`, `feat(go): stop run`.

### Task 24: Run route

**Files:** Modify stub `internal/api/workbookrun.go`; tests `internal/api/workbookrun_test.go`

**Interfaces:** Consumes `runplan.Build`, `billing.CheckAndDebit`, `queue.Enqueue`, `ratelimit`. Produces `POST /{id}/run` `RunWorkbookResponse` (`status`, `total_jobs`, `message`, remaining fields in Python order), limited 20/min, editor gate; one transaction: plan, debit (`run:<run_id>`), enqueue `run_workbook` with `executor` from the plan and `workspace_id`; 402 detail shape from Global Constraints; `skipped` body when there are no columns; stale body fields equal fixtures.

- [ ] **Step 1: Write failing tests**: `TestRunMatchesFixtures`, `TestRunDebitAndEnqueueAtomic` (enqueue error rolls the debit back), `TestRunInsufficientCredits402NothingWritten`, `TestRunReplayedRunIDChargesOnce`, `TestRunBillingOffNeverTouchesLedger`, `TestRunRateLimit20PerMinute`, `TestRunCycle422`, `TestRunForeignWorkbook404`, `TestRunExecutorFollowsCohort`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): run route (RED)`, `feat(go): run route with credit debit`.

### Task 25: Cell run route

**Files:** Modify stub `internal/api/workbookcell.go`; tests

**Interfaces:** Consumes `workbook.EnrichCell` (exported from the worker package in this task as a thin wrapper over the existing cell code; no behaviour change), `providerrun`. Produces `POST /{id}/rows/{row_id}/cells/{col_id}/run` body `{force}` (limit 120/min): in-request run of one cell, returns the fresh cell (shape equal to fixtures), `{"skipped": true}` for a complete `output` cell without `force`; `output` columns are executed by Python, so this route proxies to the Python API when the column type is `output` (documented; nginx can not split on body) and returns its response unchanged; same spend-attempt key as the queued run so a race never double-spends.

- [ ] **Step 1: Write failing tests**: `TestCellRunMatchesFixtures`, `TestCellRunSkipsCompleteOutputWithoutForce`, `TestCellRunRaceWithQueuedRunSpendsOnce`, `TestCellRunRateLimit120`, `TestCellRunLegacyLeadRowResolution`, `TestCellRunForeignRow404`, `TestCellRunCancelOnClientDisconnect`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): cell run route (RED)`, `feat(go): run a single cell in-request`.

### Task 26: Sources add and run

**Files:** Modify stub `internal/api/workbooksources.go`; tests

**Interfaces:** Produces `POST /{id}/sources` (`SourceColumnRequest`; `people_search` guarded by `PEOPLE_SEARCH_SOURCE_ENABLED` -> 403 with Python's detail; `validate_people_search_config` ported as `internal/runplan/peoplesearch.go` with vectors; row lock on the workbook; appended `source` column shape equal to fixtures) and `POST /{id}/sources/{col_id}/run` (limit 20/min; enqueues `source_workbook` with `executor='python'` and Python's payload keys; the scrapers run in Python, decision D6). `GET /sources/{col_id}/preview` stays on Python.

- [ ] **Step 1: Write failing tests**: `TestAddSourceMatchesFixtures`, `TestPeopleSearchDisabled403`, `TestPeopleSearchConfigVectors`, `TestAddSourceConcurrentAppendsBoth` (row lock), `TestSourceRunEnqueuesPythonExecutor`, `TestSourceRunRateLimit`, `TestSourceForeignWorkbook404`, `TestPreviewNotRegisteredInGo`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): source column routes (RED)`, `feat(go): add source column and enqueue source run`.

### Task 27: Refresh and refresh-policy routes

**Files:** Modify stub `internal/api/workbookrefresh.go`; tests

**Interfaces:** Consumes `refresh.SetPolicy`, `queue.Enqueue`. Produces `PUT /{id}/refresh-policy` (`RefreshPolicyRequest` defaults: `enabled true`, `on_signal []`, `staleness_ttl_days {}`; 404 with the service's `error` string) and `POST /{id}/refresh` -> `{"status":"refreshing"}` enqueuing `refresh_workbook` `{workbook_id, reason:"manual", workspace_id}` with `executor` per cohort.

- [ ] **Step 1: Write failing tests**: `TestRefreshPolicyMatchesFixtures`, `TestRefreshPolicyDefaults`, `TestRefreshPolicyUnknownWorkbook404`, `TestRefreshEnqueuesWithWorkspaceID`, `TestRefreshViewerForbidden`, `TestRefreshForeignWorkbook404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): refresh routes (RED)`, `feat(go): refresh policy and manual refresh`.

### Task 28: Migrate route

**Files:** Modify stub `internal/api/workbookmigrate.go`; tests

**Interfaces:** Produces `POST /{id}/migrate` (`workbooks.py:2370-2455`): snapshot v1 leads into `workbook_rows` in one transaction, already-migrated response unchanged, row `data` key order and positions equal to fixtures, `updated_at` stamped.

- [ ] **Step 1: Write failing tests**: `TestMigrateMatchesFixtures`, `TestMigrateAlreadyMigratedIsIdempotent`, `TestMigrateAtomicOnFailure`, `TestMigrateTenantIsolation`, `TestMigrateLargeWorkbookBatches` (10,000 leads, bounded memory).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): migrate route (RED)`, `feat(go): migrate v1 workbook to rows`.

### Task 29: Workbook WebSocket

**Files:** Modify stub `internal/api/workbookws.go`; `go.mod`/`go.sum` (pin `github.com/coder/websocket`); tests

**Interfaces:** Consumes `identity.AuthenticateQueryTokenCtx`, `tenancy.InWorkspace`, Redis client, `Deps.WSMaxMessageBytes`. Produces `GET /api/workbooks/{id}/ws?token&workspace_id` upgrade: authorise (token, workspace membership, SSO enforcement, workbook exists in that tenant) before accept, else close `4403`; subscribe `workbook:<id>`, relay each message as a text frame byte-for-byte; client `{"type":"ping"}` -> `{"type":"pong"}`; invalid JSON frames are ignored like Python's try/except-free path would crash (hardening: ignored, documented); Redis unavailable -> echo/pong mode; read limit `WS_MAX_MESSAGE_BYTES`; disconnect unsubscribes and ends both goroutines; counted by the SSE-style stream cap (`SSE_MAX_STREAMS`).

- [ ] **Step 1: Write failing tests**: `TestWSRejectsMissingTokenOrWorkspace4403`, `TestWSRejectsNonMemberAndForeignWorkbook4403`, `TestWSRelaysRedisPayloadsVerbatim`, `TestWSPingPong`, `TestWSRedisDownEchoMode`, `TestWSOversizedFrameClosed`, `TestWSDisconnectReleasesSubscription` (goroutine count back to baseline), `TestWSInvalidJSONIgnored`, `TestWSStreamCap503`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook websocket (RED)`, `feat(go): workbook websocket relay`.

### Task 30: Ingest routes

**Files:** Create `internal/db/queries/ingest.sql`; modify stub `internal/api/ingestroutes.go`; tests

**Interfaces:** Produces `POST /api/v2/workbooks/{id}/ingest-token` (editor; rotation revokes the previous token in the same transaction; plaintext returned once; 404 `Workbook not found` for foreign ids; `_require_enabled` flag answers as Python) and `POST /api/v2/workbooks/{id}/rows/ingest` (`optional_session_workspace` or ingest token from `Authorization: Bearer` / header per `_extract_ingest_token`; 401 `Missing credentials`, token failures use the service's strings; 413 `too many rows: N > MAX per request`; `_map_row`/`_key_map`/`_identity` dedupe; token hash compare constant-time; token bound to its workbook and workspace).

- [ ] **Step 1: Write failing tests**: `TestIngestTokenMatchesFixtures`, `TestIngestTokenRotationRevokesPrevious`, `TestIngestWithSessionNeedsTablesWrite`, `TestIngestWithTokenOtherWorkbookRejected`, `TestIngestMissingCredentials401`, `TestIngestTooManyRows413`, `TestIngestMapsAndDedupes`, `TestIngestDisabledFlagResponse`, `TestIngestTokenCompareConstantTime`, `TestIngestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): ingest routes (RED)`, `feat(go): ingest token create and rotate`, `feat(go): row ingest with session or token`.

### Task 31: Recompute under billing

**Files:** Modify `internal/wbcrud/recompute.go`, `internal/wbcrud/recompute_test.go`

**Interfaces:** Consumes `runplan.Build`, `billing.CheckAndDebit` (Tasks 6, 19, 24). Produces: with `BillingEnabled`, plan 04's `Unavailable()` path is replaced by the real run start (same plan, debit, enqueue as the `run` route in one transaction; 402 surfaces as Python's recompute result for insufficient credits, read from `workbooks.py` recompute handler and pinned by fixture); `Unavailable` remains only when `GO_RUN_ROUTES_ENABLED` is false.

- [ ] **Step 1: Write failing tests**: `TestRecomputeWithBillingDebitsOnce`, `TestRecomputeInsufficientCreditsResult`, `TestRecomputeBillingOffUnchanged`, `TestRecomputeUnavailableOnlyWhenRoutesDisabled`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): recompute with platform billing (RED)`, `feat(go): recompute starts runs under billing`.

### Task 32: Contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf`, `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations)
- Create: `testdata/contract/retained_enrichment_routes.txt`, `docs/go-port/runbooks/enrichment-cutover.md`, `internal/api/enrichment_coverage_test.go`

**Interfaces:** Consumes everything. Produces per-route nginx blocks (retained paths first, always `api_backend`: `generate-column`, `meta/provider-accuracy`, `sources/*/preview`; ported paths follow the canary bucket; the WebSocket location sets `proxy_http_version 1.1`, `Upgrade`/`Connection` headers and `proxy_read_timeout 1h`; `POST /run` and cell run are sticky per workspace during canary so debit and enqueue are never split across stacks); runbook: prerequisites (`PG_LEAD_STORE`, plan 03 secrets in Go or `PY_WORKER_URL` reachable, Redis, the Alembic revision on all Python workers before `GO_EXECUTOR_ENABLED`), flags order (`GO_RUN_ROUTES_ENABLED` -> cohort executor -> `GO_EXECUTOR_COLUMN_TYPES` -> `GO_EXECUTOR_SIGNAL_SCAN`), rollback (bucket 0, cohort empty, hand-back), billing cutover check, deviations list (D2-D7, atomic debit+enqueue, workspace-scoped cache, WS invalid-JSON handling).

- [ ] **Step 1: Write failing tests** `TestEveryEnrichmentRouteIsPortedOrRetained` (parses `routers/{workbooks,ingest,meta,signals}.py`; every route is in the Go patterns, in plan 04/05's lists or in `retained_enrichment_routes.txt`; expected 17 ported + 3 retained here), `TestRetainedRoutesNotRegisteredInGo`, `TestEveryProviderPortedOrListed`, `TestEveryJobTypeHandledOrPython` (the 3 ported job types are dispatched, the other 14 are not).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3:** record missing fixtures, write the nginx blocks, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (retained paths always `py`; `/api/workbooks/x/run` and `/ws` follow the bucket; `/sources/c/preview` stays `py`) -> PASS.
- [ ] **Step 5: Commit** `test(go): enrichment route coverage (RED)`, `feat(infra): nginx per-route split for enrichment`, `docs(go-port): enrichment cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row "run_workbook, refresh, signal_scan, provider_runner, ~20 HTTP-only providers, spend/receipts, email verify": run_workbook (19-21), refresh (16, 22, 27), signal_scan (17, 22), provider_runner (18), providers (13, 14), spend/receipts (existing, parity kept in 21), email verify (15), cache (2, 4, 11). Retained-route list from plans 04/05: `run` 24, `stop` 23, cell run 25, `sources*` 26 (+preview retained, D6), `refresh`/`refresh-policy` 27, `run/estimate` 23, `migrate` 28, `provider-stats` 23, `provider-accuracy` retained (D3), `generate-column` retained (D2), WebSocket 29, 2 ingest routes 30, debit 6/24, recompute under billing 31, `signals/sources` 17, job ledger D1.
- **Consistency:** `billing.CheckAndDebit` (6) used in 24 and 31; `runplan.Build` (19) in 24 and 31; `providers.Provider` (13) in 14, 15, 18; `pyworker.Call` (12) in 15, 17, 18, 21; `queue.EnqueueOnce` (5) in 16, 17; `colgraph.Order` (9) in 19, 20.
- **Review Focus:** each line has its tests in Tasks 6, 24 (1); 12, 20, 21, 25 (2); 10, 11, 13-15 (3); 9, 10 (4); 29 (5); 17 (6).
- **Proportion:** interfaces, error strings, test names and file ownership carry the decisions; provider and handler bodies are left to the implementer and to the recorded transcripts.

## Open decisions (defaults apply unless overridden)

- **D1 Job ledger.** The `collect`/`bulk_enrich` ledger (`jobs`, `job_stages`, `activity_log`, `llm_usage` in per-workspace SQLite `leads.db`) is written only by the Python collection runner and read by `/api/jobs*`. Moving it needs new Postgres tables plus a rewrite of `job_runner.py` writers, which is the leadgen collection plan (inventory row 13). Default: not moved here; `collect`, `/api/jobs*`, `lead/{id}/enrich` stay on FastAPI. This plan only enqueues Python-executor jobs through the Postgres `jobs` queue, which is already Postgres.
- **D2 `generate-column`.** `nl_column.generate_column` calls the LLM (`leadgen.llm`), so it belongs to the copilot/LLM plan (inventory row 11), not here. Default: retained on Python, listed in `retained_enrichment_routes.txt`; revisit when that plan defines the LLM boundary (it can reuse `pyworker`).
- **D3 `meta/provider-accuracy`.** Runs the offline accuracy/freshness eval harness over a Python-only golden dataset (and `?persist=true` writes the ledger). Default: retained on Python; `provider-stats` (DB read) is ported.
- **D4 Debit atomicity.** Python commits the debit before the enqueue (enqueue failure loses credits, a client retry double-charges). Default: Go debits and enqueues in one transaction; documented deviation.
- **D5 Provider cache.** The SQLite `enrichment_cache.db` cannot be shared with Go; Python's key has no workspace component. Default: new Postgres tables (Alembic, Tasks 2 and 4) keyed by workspace with RLS, so no cross-tenant cache hit; Python keeps its own SQLite cache for Python-run cells; documented deviation.
- **D6 Sources.** `POST /sources` (config validation, column append) and `POST /sources/{col}/run` (enqueue only) are ported; the run itself (`source_workbook`) and `GET /sources/{col}/preview` execute scrapers and stay Python. Default: as stated.
- **D7 WebSocket library.** Go stdlib has no WebSocket server. Default: add `github.com/coder/websocket` pinned (the only new module; `person_intel` and scraper WebSockets in the copilot plan can reuse it). Alternative: keep the workbook WS on Python until that plan.
- **D8 Python boundary transport.** Default: synchronous HTTP JSON `POST /internal/provider-call` with bearer token and Python-side process kill (no gRPC/protobuf), credentials resolved on the Python side so secrets never cross the wire. Output columns (destinations) and any job containing one stay on Python until plan 09.
- **D9 Rollout.** Default: all new Go paths off (`GO_RUN_ROUTES_ENABLED`, `GO_EXECUTOR_COLUMN_TYPES`, `GO_EXECUTOR_SIGNAL_SCAN` false); canary by workspace cohort with sticky `run` routing; unavailable `pyworker` hands the job back to Python rather than failing cells.

## User decisions

(none yet; the defaults above apply until the user objects)
