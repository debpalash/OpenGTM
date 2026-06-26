<!-- Auto-generated sourcing P2/P3 spec (2026-06-26). Effort: medium. -->

# Mini-Spec: Source Health-Check + Auto-Disable

## Goal
Add a platform-global, scheduled health-check that probes every `source_registry.SOURCES` `site:` source, tracks a rolling yield per source, **auto-disables chronically-dead sources** (and auto-re-enables on recovery) so live collection jobs stop wasting their bounded per-job source budget on sources that never return data. The mechanism must be conservative enough not to disable sources that are merely suffering transient DuckDuckGo/proxy failures, and must not touch scoring/dedup/pipeline behavior.

> Critical adversarial note up front: the report this item cites is **stale**. `docs/data-source-test-report.md:3-10` records that after the proxy-pool fix in `services/leadgen/proxy_client.py`, **91/91 registry sources return data (100%)** — the original "crunchbase/linkedin/clutch/indeed/naukri/G2/capterra ≈ 0" was caused by dead SOCKS proxies serving every search a `ConnectError`, **not** dead sources. The report explicitly warns "Empty ≠ permanently dead. A 0 can be transient DDG rate-limiting/dead proxy" (`:47`). The entire design below is therefore built around **not** repeating that mistake: auto-disable must be slow, multi-query, and gated by a systemic-outage guard, or it will disable healthy sources the next time the proxy pool hiccups.

## Grounded current state (file:line)

**Source registry** — `apps/api/services/leadgen/source_registry.py`
- `SOURCES: List[Dict]` is a 91-entry in-code list (`:42-622`). Each dict has `name/label/region/category/query_templates/site_domain/extract_from_listing/priority/enabled` (schema documented `:30-41`). `enabled` is a **static code-level default**, not runtime state.
- API: `get_all_sources(region, category, enabled_only)` (`:627`), `get_source(name)` (`:643`), `build_queries(source, query, city)` (`:656`), `get_source_summary()` (`:675`). There is **no** `search_source` despite the docstring (`:13`) — the actual search lives in job_runner.

**Live production path** — `apps/api/services/leadgen/job_runner.py`
- `_search_registry_sources(job_id, query)` (`:1534`) is the only consumer in leadgen: `get_all_sources(region)` + global merge (`:1547-1555`), hard-skips `{clutch, goodfirms, ambitionbox, linkedin_companies}` (`:1558-1559`, handled by dedicated strategies), then **filters by the runtime toggle** `get_source_enabled(s["name"])` (`:1563-1564`), then **caps at `REGISTRY_SOURCE_CAP=20` by priority** (`:1573-1576`), runs in parallel batches of 10 (`:1593-1699`), 1 query/source via `_ddg_search(dq, max_results=8)` (`:1604`).
- `_ddg_search(query, max_results)` (`:52`) → `asyncio.wait_for(to_thread(_ddg_text_sync), 25s)`; `_ddg_text_sync` (`:45`) uses `proxy_client.get_ddgs()` (the resilient direct/proxy/multi-engine client). `_is_junk_url` (`:75`) drops asset/ad URLs.
- The report's test harness used *exactly* `build_queries(source, "software companies","Bangalore")[0]` → `_ddg_search(q, max_results=8)` (`docs/data-source-test-report.md:23-25`), so the probe can faithfully reproduce the live signal.

**Runtime per-source toggle (operator intent)** — `apps/api/routers/settings.py`
- `DATA_SOURCES` (`:770-912`, 96 entries; ids align to registry `name`s) drives the Sources UI. `get_source_enabled(source_id)` (`:915`) reads `SOURCE_{ID}_ENABLED` from the **settings SQLite DB** (`_db_get` `:49`, `_db_set` `:62`), falling back to `default_enabled`. `PUT /sources/{id}` (`toggle_source`, `:942`) writes it. `GET /sources` (`:925`) and `GET /sources/registry` (`:957`) expose state. **This settings store is global (not workspace-scoped) and per-instance (SQLite), not shared across worker replicas.**

**Scheduling / durable-job infrastructure** — the pattern to mirror
- Durable Job queue: `apps/api/services/queue_service.py`. `register_handler(type, handler)` (`:62`), `add_job` (`:67`), atomic claim `FOR UPDATE SKIP LOCKED` (PG) / guarded UPDATE (SQLite) (`:131/:159`), per-type timeout `JOB_TIMEOUTS` (`:32`), retry/backoff + heartbeat reaper (`:333-410`). Global singleton `queue_service` (`:420`).
- Handler registration + cold-start bootstrap happen in `apps/api/main.py` lifespan (`:119-190`). Each recurring subsystem (`signal_scan`, `trigger_eval`, `watch_poll`, …) registers a handler and calls a `bootstrap_*()` on startup behind a feature flag.
- **Recurring self-reschedule + non-RLS mirror pattern**: `automations/engine.py` — `compute_next_run(anchor, interval)` (`:397`), `_interval_delta` (`:387`), single-flight `_enqueue_schedule_if_absent` keyed by `fire_key` (`:411`), `bootstrap_schedules()` reads a non-RLS mirror and re-enqueues due jobs on cold start (`:500`).
- **The exact auto-disable/backoff state machine to reuse** is in `poller/engine.py` `_reschedule_watch` (`:257-286`): increment `consecutive_failures`, exponential backoff, and on `>= INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` set `enabled=False` + `last_error="auto_disabled"`. Config flags live in `core/config.py` (`INTENT_POLLER_ENABLED`, thresholds, intervals `:170-176`).

**Tenancy / persistence facts**
- Leadgen shared tables (`orm_models.py`, RLS) are tenant-scoped; non-RLS schedule mirrors live in `poller/models.py` / `automations/models.py`. **Source health is platform-global, not per-tenant** — `SOURCES` and `get_source_enabled` are global today, so health state must be global too (no `workspace_id`).
- Alembic head is **`e5f6a7b8c9d0`** (workbooks_rls; the MEMORY note `b2c3d4e5f6a7` is stale). `IS_SQLITE` / `SessionLocal` from `apps/api/database.py` drive dialect.

## Design

### Health signal
A scheduled `source_health_check` job (global singleton, not per-tenant) probes **every** source in `SOURCES` (regardless of enabled/health state — disabled sources must keep being probed for recovery). For each source it runs `M` **canary queries** through the live path:

```
for cq in CANARY_QUERIES:                       # e.g. ("software companies","Bangalore"), ("marketing agency","New York")
    q = build_queries(source, cq.query, cq.city)[0]
    results = await _ddg_search(q, max_results=8)
    n = len([r for r in results if not _is_junk_url(r.get("href",""))])
probe_yield = max(n over canary queries)        # "did this source return anything usable on ANY canary?"
probe_zero  = (probe_yield == 0)
```

Multiple canaries across geos directly counters the report's documented query-sensitivity false negatives (forbes/quora 1/4, reddit 2/4, `:13-17`). Yield = max over canaries (a source is "alive" if any canary works). This reuses the resilient `get_ddgs` client and the production transform, so the signal matches what live jobs experience.

### Rolling metric & state machine
Per source row in `source_health`:
- `state`: `healthy | degraded | auto_disabled`
- `consecutive_zero` (int), `consecutive_nonzero` (int)
- `last_yield` (int), `ewma_yield` (float), `last_probe_at`, `last_ok_at`
- `disabled_at`, `disabled_reason`, `manual_override` (bool — operator force-enabled; health may never auto-disable it)
- `updated_at`

Transitions (per probe, evaluated only when the run is **not** flagged as a systemic outage — see guard below):
- nonzero → `consecutive_zero=0`, `consecutive_nonzero+=1`, `last_ok_at=now`. If `state==auto_disabled` and `consecutive_nonzero >= RECOVER_THRESHOLD` → `state=healthy`, clear `disabled_*`.
- zero → `consecutive_nonzero=0`, `consecutive_zero+=1`. If `consecutive_zero >= DEGRADE_THRESHOLD` → `state=degraded`. If `consecutive_zero >= DISABLE_THRESHOLD` and not `manual_override` → `state=auto_disabled`, `disabled_at=now`, `disabled_reason="zero_yield"`.

Defaults (conservative, given "empty≠dead"): `DEGRADE_THRESHOLD=3`, `DISABLE_THRESHOLD=6`, `RECOVER_THRESHOLD=2`. With a daily probe that is ~6 consecutive dead days (each across M canaries) before a source is disabled, and 2 good days to recover — slow enough to ride out proxy/DDG incidents, fast enough to matter.

### Systemic-outage guard (the key safeguard)
After probing all sources in a run, compute `zero_ratio = zeros / total`. If `zero_ratio >= SOURCE_HEALTH_OUTAGE_RATIO` (default `0.6`), treat the run as **infra failure** (DDG rate-limit / proxy pool down — the exact failure mode in the report): **do not apply any zero transitions** for that run (record `last_probe_at` + a `last_outage_at` marker, log a warning, optionally emit an alert), and reschedule normally. Nonzero results may still advance recovery counters. This single guard is what prevents the proxy-bug class of mass false-disable.

### Effective-enabled gate (additive, non-destabilizing)
Keep the operator toggle (`get_source_enabled`) as **authoritative intent**; health is a *separate* layer. Add a helper in the health module:

```
def is_health_disabled(name: str) -> bool   # True only if flag ON and row.state == 'auto_disabled'
```

`_search_registry_sources` filter becomes (behind the flag): `enabled = get_source_enabled(name) and not is_health_disabled(name)`. Missing health row → **fail-open** (treated healthy). Flag OFF → helper returns False for everyone → zero behavior change.

### Scheduling
- New durable job type `source_health_check`, handler `handle_source_health_check(job_id, payload)`, registered in `main.py`.
- Single global schedule (no per-tenant fan-out). Self-reschedules at the end of each run via single-flight `_enqueue_health_if_absent(fire_key=f"source_health:{next_run_iso}")` keyed off `compute_next_run(anchor, SOURCE_HEALTH_INTERVAL)` (reuse `automations.engine.compute_next_run`). `JOB_TIMEOUTS["source_health_check"]` set generously (e.g. 1800s) since it makes ~91×M bounded DDG calls.
- `bootstrap_source_health()` on cold start (behind flag): if no pending/processing `source_health_check` job exists, enqueue one. No mirror table strictly needed (singleton, fixed anchor) — single-flight on `fire_key` + "enqueue if none pending" is sufficient and matches `bootstrap_signal_scan`'s shape.
- Bounded cost: probe in batches with `asyncio.sleep` between batches (mirror `_search_registry_sources` batching `:1593-1699`), low concurrency, `max_results=8`. Default interval **daily** to keep added DDG volume small and avoid the probe itself amplifying rate-limiting.

## File-by-file changes
1. **NEW `apps/api/services/leadgen/source_health.py`** — `SourceHealth` ORM model (non-RLS, global, PK = source `name`); `CANARY_QUERIES`; `handle_source_health_check`; `_run_probe(source)`; `_apply_transition(row, probe_yield, outage)`; `is_health_disabled(name)`; `health_summary()`; `bootstrap_source_health()`; `_enqueue_health_if_absent`. Imports `build_queries`, `_ddg_search`, `_is_junk_url` from existing modules — no logic duplication.
2. **NEW migration `migrations/versions/<rev>_source_health.py`** (`down_revision="e5f6a7b8c9d0"`) — create `source_health` table (columns above; **no `workspace_id`, no RLS** — it is global infra state). Backward-safe additive DDL.
3. **EDIT `apps/api/services/leadgen/job_runner.py:1563-1564`** — extend the filter to `get_source_enabled(name) and not is_health_disabled(name)`; surface health-disabled sources in the existing skip-preview `progress.emit` (`:1577-1585`) so operators see *why* a source was skipped.
4. **EDIT `apps/api/main.py:142-190`** — `register_handler("source_health_check", handle_source_health_check)` and `bootstrap_source_health()` in a try/except behind the flag (mirror `:160-190`). Add timeout entry in `queue_service.JOB_TIMEOUTS`.
5. **EDIT `apps/api/core/config.py`** (near `:170`) — `SOURCE_HEALTH_ENABLED=False`, `SOURCE_HEALTH_INTERVAL="daily"`, `SOURCE_HEALTH_DEGRADE_THRESHOLD=3`, `SOURCE_HEALTH_DISABLE_THRESHOLD=6`, `SOURCE_HEALTH_RECOVER_THRESHOLD=2`, `SOURCE_HEALTH_OUTAGE_RATIO=0.6`, `SOURCE_HEALTH_PROBE_CONCURRENCY=8`.
6. **EDIT `apps/api/routers/settings.py`** — `GET /sources/health` (returns `health_summary()`); merge `state/last_yield/last_ok_at/disabled_reason` into `GET /sources` (`:925`) so the UI shows a health badge; in `toggle_source` (`:942`), when an operator manually enables a source, set `manual_override=True` + clear `auto_disabled` (operator intent wins). Optional `POST /sources/{id}/health/reset`.
7. **NEW tests** `apps/api/tests/test_source_health.py`.

## Data / persistence + tenancy
- **Global, non-RLS** `source_health` table keyed by source `name`; no `workspace_id` (sources are shared infra, exactly like `get_source_enabled`). Lives in the same DB as the Job queue (`SessionLocal`), so it is shared across worker replicas — a strict improvement over the per-instance settings SQLite for replica correctness. Operator toggle stays where it is (`SOURCE_{ID}_ENABLED` in settings SQLite); the two layers are ANDed, never merged.
- Bounded memory/cost: one row per source (≤~100 rows), no unbounded growth (rolling counters + EWMA, not an event log). Probe volume is fixed (`91 × M` capped DDG calls/day).
- Dialect-portable (PG + SQLite) so dev/test work unchanged.

## Backward-compat & non-destabilization
- **Default OFF** (`SOURCE_HEALTH_ENABLED=False`): no handler bootstrap, gate helper returns False for all → `_search_registry_sources` behaves byte-for-byte as today.
- **Fail-open**: missing/unknown health row ⇒ source treated healthy; a bug in the health table can never silently blind the engine.
- **No scoring/sourcing-logic changes**: only the source-selection *membership* filter is touched, and only behind the flag. `scoring.py`, `pipeline.py`, `dedup.py`, `lead_validator.py` untouched. The existing `clutch/goodfirms/ambitionbox/linkedin_companies` hard-skip (`:1558`) and `REGISTRY_SOURCE_CAP` (`:1573`) are preserved.
- **Operator authority preserved**: manual enable sets `manual_override` so health can't fight the operator; manual disable is independent of health.
- **Multi-replica safe**: single-flight `fire_key` dedup (reused pattern) means only one replica runs a given probe cycle; the atomic Job claim prevents double execution.
- **Anti-flap + outage guard** specifically neutralize the report's proxy-pool false-zero failure mode.

## Feature-flag / rollout
1. Ship migration + code with `SOURCE_HEALTH_ENABLED=False`. No-op in prod.
2. Enable probe only (gate left disconnected) by temporarily setting `DISABLE_THRESHOLD` very high or shipping the gate in "observe" mode: record state + expose `/sources/health` but do NOT filter. Watch 1–2 weeks of probe data to validate thresholds against real DDG/proxy variance.
3. Flip on the gate (lower `DISABLE_THRESHOLD` to 6). Monitor `last_outage_at` frequency and auto-disable churn.
4. Roll back instantly by setting `SOURCE_HEALTH_ENABLED=False`.

## Test plan (`PYTHONPATH=. uv run --group dev python -m pytest`)
- Transition unit tests: 6 consecutive zeros → `auto_disabled`; 2 nonzeros from disabled → `healthy`; degrade at 3.
- Outage guard: a run with `zero_ratio>=0.6` applies **no** zero transitions (no false disable) even with all-zero probes.
- Gate: `is_health_disabled` only filters when flag ON and state `auto_disabled`; missing row fails open; flag OFF → no filtering (assert `_search_registry_sources` source set unchanged).
- Manual override: operator enable on an `auto_disabled` source clears it and pins `manual_override`.
- Scheduling: `bootstrap_source_health` enqueues exactly one job; second call is single-flight no-op; handler self-reschedules.
- Probe yield: monkeypatch `_ddg_search` to fixtures; assert junk-filtered count and max-over-canary.
- Dialect: run on live PG (and SQLite) per repo convention.

## Acceptance criteria
1. With `SOURCE_HEALTH_ENABLED=False`, the registry source set selected by `_search_registry_sources` is identical to pre-change (regression-guarded by test).
2. A `source_health` row is created/updated for every source in `SOURCES` on each probe run.
3. A source returning zero usable results on all canaries for `DISABLE_THRESHOLD` consecutive non-outage runs transitions to `auto_disabled` and is excluded from live jobs (flag ON).
4. An `auto_disabled` source returning nonzero on `RECOVER_THRESHOLD` consecutive runs transitions back to `healthy` and re-appears in live jobs.
5. A run where `zero_ratio >= SOURCE_HEALTH_OUTAGE_RATIO` records no zero transitions and logs an outage; no source is disabled by that run.
6. Operator manual enable of an `auto_disabled` source immediately re-enables it and prevents future auto-disable (`manual_override`).
7. `GET /sources/health` returns per-source `state/last_yield/last_ok_at/disabled_reason`; `GET /sources` shows the health badge.
8. The health-check is a single global durable job that self-reschedules at `SOURCE_HEALTH_INTERVAL`, survives restart via bootstrap, and runs at most once per cycle across replicas.
9. No changes to scoring/dedup/validator/pipeline outputs (existing suites green on live PG).
10. Probe DDG call volume per cycle is bounded to `≤ |SOURCES| × len(CANARY_QUERIES)` and batched with sleeps.

## Out of scope
- Passive per-source yield telemetry from real jobs (phase 2; would feed the same table without extra DDG load).
- Per-tenant source health / per-tenant overrides.
- Health for the dedicated-strategy scrapers (google_maps, linkedin, crunchbase scrapers, job_boards) — this item is the DDG `site:` registry sources only.
- Replacing dead `site:` sources with official APIs (separate backlog item; report `:154`).
- Auto-tuning thresholds / anomaly detection beyond the fixed counters + outage guard.
