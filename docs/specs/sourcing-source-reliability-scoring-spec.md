<!-- Auto-generated sourcing P2/P3 spec (2026-06-26). Effort: medium. -->

## Mini-spec: Source-reliability scoring in ranking

### Goal
Weight each lead-source's historical reliability (hit-rate, dedup-survival, validation-pass-rate) into the lead score so chronically empty/noisy sources (per `docs/research/data-source-test-report.md`: crunchbase/linkedin/clutch/indeed/naukri/G2/capterra ≈0 results) down-rank their leads and reliable sources up-rank — as a **bounded, flag-gated nudge** that never overturns ICP fit.

---

### Grounded current state (file:line)

**Scoring is pure ICP, source-blind.** `score_lead()` (`apps/api/services/leadgen/scoring.py:14-113`) sums `SCORING_WEIGHTS` (`config.py:81-91`) over data-completeness / company-fit signals and caps at 100. Nothing reads `lead.source`. `get_tier()` (`scoring.py:116-121`) maps to tiers from `SCORE_TIERS` (`config.py:93-98`). Batch re-score: `score_and_update_db()` (`scoring.py:147-174`).

**Provenance per lead.** `Lead.source` (`models.py:61`) is stamped at scrape time with the originating channel: `"web_search"` (`job_runner.py:919,957,975`), `"directory"` (`1127,1222`), `"linkedin"` (`1305`), `"job_board"` (`1398`), `"review_site"` (`1515`), and `"registry:<name>"` for the 91-source registry (`job_runner.py:1659`, names from `source_registry.py:42-622`). Google Maps leads come from `scrapers/google_maps`.

**CRITICAL provenance gap:** at store time `job_runner.py:768` does `lead.source = f"job:{job_id}"`, **overwriting the channel before persistence**. So the channel only exists *in-flight* during the job; the persisted `leads.source` (`orm_models.py:110`) is `job:<id>`. Reliability must therefore be computed/applied *during* the job (where `lead.source` is still the channel — scoring at `job_runner.py:363/370` runs long before the 768 overwrite), and per-source outcomes must be tallied in-flight.

**Outcome stages we can measure per source (all in-flight):**
- emitted: `all_leads` after gather (`job_runner.py:280-283`)
- survived dedup: `unique` after `deduplicate_leads` (`job_runner.py:314`, impl `pipeline.py:15-35`)
- validation-pass: light validate `valid_leads` (`job_runner.py:297`) and post-enrich `validate_and_clean_leads` (`job_runner.py:449`, validator `lead_validator.py:190-310,407-421`)

**Strong existing precedent to mirror — enrichment-provider reliability ranking.** `ProviderStat` (`apps/api/services/workbook/planner_models.py:13-62`) is a **global** ledger (`UniqueConstraint("provider","field")`, no `workspace_id`) of `attempts/hits` → `hit_rate` (property `:40-41`), plus a learned `accuracy_score`. The planner ranks providers by `_score()` (`planner.py:102-112`) using `hit_rate` with an **optimistic prior of 0.5 until `attempts>=3`** and bends it by `correctness_prior()` (`planner.py:75-99`) — a multiplier in `[1-W, 1]`, `W=CORRECTNESS_WEIGHT=0.6` (`planner.py:72`), that **returns 1.0 (no effect) when never evaluated** (graceful default). Upsert-with-atomic-increments is `record_attempt()` (`planner.py:131-153`). Migration precedent: `f3b30195d51a_add_provider_accuracy_columns_to_.py`. This is the exact shape to reuse.

**Migration head:** chain tip is `e5f6a7b8c9d0_workbooks_rls` (verified by walking `revision`/`down_revision` across `migrations/versions/*`). (Memory's `b2c3d4e5f6a7` is stale.)

**Source toggles already exist** (`routers/settings.py:915-922` `get_source_enabled`) — reliability is *separate* from enable/disable and will not auto-disable sources in v1.

---

### Design

**1. Per-source rolling reliability metric `r ∈ [0,1]`.** Three components, each a ratio in `[0,1]`:
- `hit_rate = runs_with_output / runs` (did the source return anything for a query)
- `dedup_survival = survived_dedup / emitted`
- `validation_pass = validated / emitted`

Combine: `r = w_hit*hit_rate + w_dedup*dedup_survival + w_val*validation_pass` (default `0.5/0.25/0.25`). Persist as a **rolling EWMA** (`r_new = α*r_batch + (1-α)*r_old`, `α≈0.3`) for bounded memory and recency-bias — old behavior decays as sources recover/rot. Store cumulative counters too (for transparency/debug) but rank off the EWMA.

**2. Where stored — new global table `source_stats`** (mirrors `ProviderStat`):
`id, source (str), region (str), runs, runs_with_output, emitted, survived_dedup, validated, reliability (Float, EWMA), updated_at`; `UniqueConstraint("source","region")`. **Global, not workspace-scoped** (recommended default, matches `ProviderStat`): source health is a property of the source+region+open-web, accumulates signal fastest pooled across tenants, and the table holds **no tenant PII** (only aggregate source health), so it sits outside the RLS tenancy model cleanly. `region` is part of the key because region dominates whether a source yields (e.g. `naukri` is India-only) — `job_runner._detect_region()` (`job_runner.py:1708`) already computes it. (Per-workspace/ICP scoping = OWNER decision below.)

**3. How it feeds ranking without destabilizing** — bend, don't break, mirroring `correctness_prior`:
- New `scoring.py` helper applied as the **last step** on the already-computed score:
  `adjusted = clamp(round(base + RELIABILITY_SWING * (r - 0.5)), 0, 100)` with `RELIABILITY_SWING` small (default **8**) → at most ±4 points (since `r-0.5 ∈ [-0.5,0.5]`). ICP fit (weights of 10-20) keeps dominating; reliability only breaks ties / nudges across a tier boundary.
- **Graceful default:** if a source has `runs < MIN_SAMPLES` (default 5) → reliability returns `None` → **no adjustment** (identical to today). Same philosophy as planner's "prior until we have data."
- **Score-blind execution:** v1 reliability affects *score only*, never which sources run (registry cap stays priority-based, `job_runner.py:1571-1576`) — avoids a starvation feedback loop where a down-ranked source is never sampled again.

---

### File-by-file changes

1. **NEW `migrations/versions/<rev>_source_reliability_stats.py`** (`down_revision="e5f6a7b8c9d0"`): create `source_stats` (columns above), `UniqueConstraint("source","region")`, index on `(source, region)`. No RLS (global, non-tenant table — match `provider_stats`).

2. **NEW `apps/api/services/leadgen/source_stats.py`** — `SourceStat(Base)` ORM model + service fns (modeled on `planner.py`):
   - `normalize_source(raw: str) -> str`: canonicalize `lead.source` → ledger key (`"registry:indiamart"→"indiamart"`; `"web_search"/"directory"/"linkedin"/"job_board"/"review_site"/maps` pass through). Single source of truth for the heterogeneous strings.
   - `reliability(db, source, region) -> float | None`: EWMA if `runs>=MIN_SAMPLES` else `None`.
   - `reliability_map(db, region) -> dict[str,float]`: bulk fetch for a job.
   - `record_run(db, region, per_source_counters)`: atomic upsert + EWMA update (concurrency-safe like `record_attempt`, `planner.py:131-153`).

3. **`apps/api/services/leadgen/scoring.py`**: add optional `source_reliability: float | None = None` to `score_lead()`; new `_apply_source_reliability(score, r)` (clamped ±swing). `None` → identity (default path byte-for-byte unchanged). `score_leads()`/`score_and_update_db()` signatures unchanged (re-score path stays base-only since persisted `source` is `job:<id>`).

4. **`apps/api/services/leadgen/job_runner.py`**:
   - Tally per-source counters in-flight: emitted from `all_leads` (`:280`), survived from `unique` (`:314`), validated from post-`validate_and_clean_leads` `scored` (`:449`) — keyed by `normalize_source(lead.source)`.
   - **Final bounded re-score pass** right before store (after hiring-signals boost `:651-654`, before `:763`), gated by flag: fetch `reliability_map(region)`, recompute `lead.score`/`score_tier` via `score_lead(lead, source_reliability=...)` using the still-intact channel `lead.source`.
   - At job completion (near `:789`) call `record_run()` to persist the batch's counters.
   - Flag read once via `os.getenv` (consistent with `DDG_SEARCH_TIMEOUT`/`REGISTRY_SOURCE_CAP`).

5. **`apps/api/core/config.py`** (optional, for discoverability): add `SOURCE_RELIABILITY_RANKING: bool = False`, `SOURCE_RELIABILITY_SWING`, `SOURCE_RELIABILITY_MIN_SAMPLES` to `settings`, with env override. (Or keep pure-env to match leadgen module style — minor.)

6. **(Stretch, out-of-scope-flag) `routers/settings.py`**: read-only `/sources/reliability` endpoint surfacing `source_stats` for the Sources UI (operator visibility into which sources are dead).

---

### Data / persistence + tenancy
- New global `source_stats` table; bounded rows (≤ #sources × #regions ≈ low hundreds); bounded memory via EWMA (no per-lead history). Written once per job (one `record_run`), read once per job (`reliability_map`).
- **Tenancy:** global (no `workspace_id`), justified above (no tenant data; matches `ProviderStat`). RLS untouched.
- Idempotent/concurrent writes via unique constraint + atomic upsert (workers are stateless, `job_runner._lead_store` note `:103-116`).

### Backward-compat / non-destabilization
- **Flag default OFF** → zero behavior change; `score_lead` default arg `None` → identical scores; new table simply accumulates stats (or stays empty) and is never read.
- When ON: graceful-default (`runs<MIN_SAMPLES` → no adjustment), bounded ±4-point swing can't flip a hot lead to cold or override ICP weights, score always clamped `[0,100]`, never zeroes.
- No change to `SCORING_WEIGHTS`, dedup, validation, or source execution order. Re-score-all admin path unchanged.

### Feature-flag / rollout
1. Ship table + ledger writes with **scoring OFF** → accumulate real reliability data passively for N days (no ranking impact).
2. Inspect `source_stats` (or stretch endpoint) vs `docs/research/data-source-test-report.md` to sanity-check.
3. Flip `SOURCE_RELIABILITY_RANKING=true` in staging; compare tier distributions; tune `SWING`/weights.
4. Enable in prod. Instant rollback = flip flag off.

### Test plan (`tests/test_source_reliability.py`, run `PYTHONPATH=. uv run --group dev python -m pytest`)
- `normalize_source` mapping (registry prefix strip, channel passthrough).
- Reliability math: component ratios, weighted combine, EWMA update, `None` below `MIN_SAMPLES`.
- `_apply_source_reliability`: ±swing bounds, clamp `[0,100]`, `r=0.5`→no change, `None`→identity.
- **Flag-off regression:** `score_lead(lead)` identical with/without ledger populated (assert byte-identical scores across a fixture set).
- Ledger upsert concurrency/idempotency (mirror `test_provider_accuracy_eval.py` style; live PG).
- Job-level: synthetic counters → `record_run` → `reliability_map` → low-reliability source's lead scores lower than high-reliability peer with equal ICP fit, by ≤ swing.

### Numbered acceptance criteria
1. With flag OFF, all existing scoring/dedup/validation tests pass unchanged and scores are byte-identical to pre-change.
2. `source_stats` migration applies/rolls back cleanly on PG and SQLite from head `e5f6a7b8c9d0`.
3. A job records one `source_stats` upsert per distinct in-flight source, with correct emitted/survived/validated counters and updated EWMA reliability.
4. Sources with `runs < MIN_SAMPLES` produce no score adjustment.
5. With flag ON, two leads with identical ICP fit but sources of reliability 0.9 vs 0.1 differ in final score by `≤ RELIABILITY_SWING` and in the correct direction.
6. No final score escapes `[0,100]`; no lead is zeroed by the adjustment alone.
7. `lead.source` provenance (channel) is read before the `job:<id>` overwrite; persisted behavior of `leads.source` is unchanged.
8. Source execution set/order is unaffected by reliability in v1.

### Out of scope
- Reliability-gated source *execution* (auto-skip/cap dead sources) — v1 is score-only.
- Per-workspace/per-ICP reliability ledgers (global only).
- Adding a persisted `origin_source` column to `leads` (would let later re-scores use channel) — noted as a future enhancement; v1 works in-flight without schema churn to `leads`.
- Backfilling reliability from historical `job:<id>` leads (channel already lost).
- UI beyond the optional read-only endpoint.
