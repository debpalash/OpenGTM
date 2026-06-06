# Workbook V2 — The Lead Source Engine
## Technical Spec & Design

> Branch: `debpalash/workbook-v2`
> Status: Design / pre-implementation
> Thesis: Invert Clay. Make the **workbook itself the sourcing primitive** — a living, self-populating, signal-triggered, entity-resolved surface — not a static table you import into and enrich.

---

## 0. The One-Sentence Bet

**Clay makes you bring the rows. Yupcha's workbook _grows_ the rows** — it is a standing query over a 91-source web graph that continuously materializes entity-resolved, corroborated, agentically-enriched leads, fully self-hosted.

That combination — **live multi-source sourcing + cross-source entity resolution + agentic per-row enrichment planning, inside one open workbook** — does not exist as a single product today. That is the "industry first."

| Competitor | Sources rows? | Enriches? | Live / signal-triggered? | Entity-resolves across sources? | Self-hosted? |
|---|---|---|---|---|---|
| Clay | ❌ (you import) | ✅ | partial (Signals add-on) | ❌ | ❌ |
| Apollo | ✅ (static DB) | ✅ | ❌ | ❌ (single DB) | ❌ |
| Instantly | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Yupcha WB v2** | ✅ **live, 91 sources** | ✅ **agentic waterfall** | ✅ **cron + signals** | ✅ **canonical graph** | ✅ |

---

## 1. Where We Are Today (grounded in the code)

What already exists and works — the spec builds on these, it does not re-invent them:

| Layer | Implementation | File(s) |
|---|---|---|
| **91 sources** (directories, B2B marketplaces, review, jobs, social, gov, startup, SaaS dirs, news, ecom, freelance) | Config-driven DDG site-scoped registry. New source = new dict entry. `build_queries()` expands `{q}/{city}/{industry}`. | `source_registry.py` |
| **~30 enrichment providers** | `EnrichmentProvider` ABC (`name`, `capabilities`, `default_confidence`, async `enrich(lead)->EnrichmentResult`). Free OSS + BYOK. | `enrichment/provider.py:63`, `workbook/providers.py` |
| **Waterfall engine** | `WaterfallEnricher`: per-field ordered chain, 15s/provider timeout, stop at first `has_value`, `WaterfallLog` provenance. **Static chains** in `DEFAULT_WATERFALLS` (free-first, BYOK-last). | `enrichment/provider.py:104` |
| **Workbook v2 (Clay-style)** | Self-contained `WorkbookRow` (inline `enrichments` JSON); types `lead_field/enrichment/waterfall/ai_formula/conditional/output`; conditions gate execution to save credits. | `workbook/models.py`, `workbook/enrichment.py:108` |
| **Sourcing (today)** | `JobRunner._process_job`: 6 strategies + registry via `asyncio.gather` (180s/strategy), in-batch + cross-job dedup, scoring → leads DB. Emits real-time events on a `progress.emit` bus. **Out-of-workbook; merged in as a 2nd step.** | `job_runner.py:126`, `from-jobs` endpoint |
| **Dedup (exists, mostly unwired)** | Sophisticated `LeadDeduplicator` — Jaro-Winkler + Union-Find blocking, field weights (company .35 / domain .30 / phone .15 / email .10 / city .10), merge waterfall. **Live pipeline only calls naive name-fuzzy `deduplicate_leads(threshold=85)` + cross-job name-set.** | `services/dedup.py:272` (vs `pipeline.py:15`) |
| **Provenance (partial)** | Lead carries `source` (e.g. `registry:indiamart`, merged `a\|b\|c`), `email_provider`, `phone_provider`, `enrichment_waterfall` JSON, `enrichment_attempts`. No per-field confidence/freshness; sourcing lineage is one lossy string. | `leadgen/models.py` |
| **Signal monitor** | 7 signal types (hiring/funding/tech/news/…), **manual-trigger only** (`POST /api/signals/scan`), stored in `signals.db`. **Not linked to score or any action.** | `signals/monitor.py:185` |
| **Background jobs** | DB-polling `queue_service` worker (`Job.next_run_at`, retry/backoff, heartbeat, dead-job reaper). Only `download_link` handler registered. **This is the real job backbone — not BullMQ.** | `queue_service.py:84` |

**The gap (sharpened by intel):** sourcing is an _upstream, one-shot, out-of-workbook_ step; the connective tissue (entity resolution, per-field provenance/confidence, living refresh, cost control) either doesn't exist or exists-but-unwired. **And the execution substrate the whole vision rests on is currently broken in the running app — see §1.5.** We pull sourcing into the workbook as a first-class agentic column AND build the tissue that turns 91 disconnected scrapers into one coherent, trustworthy source engine.

---

## 1.5. Hard Prerequisites — The Execution Substrate Is Broken (must fix first)

> A live-app investigation (workbook `283139da`, "Competitor Landscape", 500 rows) found enrichment **permanently stuck in `status=running`**. None of the five pillars can ship on top of this. **This is P-1 — it blocks everything.**

Root causes (all confirmed in the running app at `/Users/user4/Desktop/yupcha/lead-data`, *not* this worktree):

1. **Queue path is dead.** `run_workbook` → `enqueue_enrichment_job` → silently degrades to `redis.lpush("yupcha:enrichment:jobs")` because **`bullmq` is not installed and no worker process is alive**. The `brpop` consumer (`worker.py`) isn't running, so queued cells sit forever. (`routers/workbooks.py:706`, `worker.py:253`)
2. **Inline fallback is non-viable at scale.** When enqueue returns falsy, `/run` falls through to `await enrich_workbook_leads(...)` **inside the HTTP request** — a fully serial double loop (`enrichment.py:356`). 500 rows × 5 cols = ~2,500 cells, each a 2–30s DDG/SMTP/scrape call, **no concurrency**. Hours of blocking work in one request; uvicorn `--reload` / client timeout kills it first.
3. **No completion guarantee / no reaper.** `wb.status` is set to `running` *before* the loop and `complete` only *after* it finishes (`workbooks.py:740`). Any timeout, disconnect, hot-reload, or one hung provider leaves the workbook **stuck `running` forever** with no heartbeat recovery.
4. **Redis pub/sub is design-only.** The per-workbook WS endpoint subscribes to `workbook:{id}` but **nothing publishes to it** — live cell/row updates don't actually flow. (`routers/workbooks.py:810`, no publisher in `enrichment.py`/`worker.py`)

**P-1 deliverable — a real execution substrate (reuse `queue_service`, do NOT depend on BullMQ):**

- Register a **`run_workbook_cell` / `run_workbook` job handler** on the existing DB-polling `queue_service` (it already has `next_run_at`, retry/backoff, heartbeat, and a dead-job reaper — exactly what's missing). This makes BullMQ optional, not load-bearing.
- **Concurrency**: process cells with a bounded `asyncio.gather` pool (e.g. 10–20) inside the worker, not one-at-a-time.
- **Status state machine + reaper**: per-row/per-cell status, workbook `running→complete` derived from cell completion counts; the existing dead-job reaper resets stuck runs.
- **Wire the publisher**: on each cell completion, `redis.publish("workbook:{id}", {...})` so the existing WS subscriber actually streams updates. This same channel carries Pillar 1's `row_added`.

This P-1 work is **load-bearing for every pillar** (source columns emit rows through it; living workbooks re-run through it; agent columns step through it). Ship it first.

---

## 1.6. Reuse Map — Build-On vs Build-New

Intel changed two earlier assumptions: entity resolution is *build-on* (a strong `LeadDeduplicator` already exists), and the job backbone is `queue_service`, not BullMQ. Net build surface:

| Pillar | Reuse (exists) | Build-new |
|---|---|---|
| **P0 Source Column** | `JobRunner` 6-strategy fan-out, `source_registry.build_queries`, `progress.emit` event bus, `from-jobs` row materialization | `source` column type + ICP→query planner; drive JobRunner from workbook ICP; write to `WorkbookRow`; `row_added` publish |
| **P1 Entity Graph** | `LeadDeduplicator` (Jaro-Winkler + Union-Find + field weights + merge), `source` `\|`-merge | **Wire it into the live path** (today only naive name-fuzzy runs); persist as `CompanyEntity`/`PersonEntity`; corroboration score; review/split |
| **P2 Provenance + Cost** | `WaterfallLog`, `{field}_provider`, `enrichment_waterfall` | Per-field `{value,source,provider,confidence,observed_at}`; extend ABC with `cost_per_lookup`/`free_tier_limit`; `ProviderStat` ledger; budget ceiling |
| **P3 Living Workbook** | `queue_service.Job.next_run_at` + retry, `signals/monitor` (7 types) | `refresh_policy`; recurring re-enqueue job type; **wire signal→score→action** (today signals touch neither) |
| **P4 Agent Column** | provider registry, browser-use infra (`bull:browser-jobs` exists) | `agent` column + per-row planner loop + reasoning trace |

---

## 2. The Five Pillars

### Pillar 1 — Source Columns (sourcing becomes a workbook primitive)

Today a workbook starts with rows. In V2, a workbook can start with **a Source Block** — a special column/node that _emits rows_ rather than enriching them.

New column type:

```python
"source": {
    "description": "Materializes NEW rows from the 91-source engine via an ICP query",
    "icon": "Radar",
    "editable": False,
    "has_config": True,   # ICP query, regions, source categories, target count, dedup policy
    "emits_rows": True,   # NEW capability flag — this column creates rows, not cells
}
```

Source-column config:

```jsonc
{
  "id": "src_1",
  "type": "source",
  "icp": {
    "description": "IT staffing companies in Bangalore, 50-500 employees",
    "industry": "IT staffing",
    "geo": ["Bangalore", "Pune"],
    "size": { "min": 50, "max": 500 },
    "keywords_any": ["recruitment", "talent", "staffing"],
    "exclude": ["consulting only"]
  },
  "channels": { "categories": ["directory","b2b_marketplace","jobs","review","startup"],
                "regions": ["india"], "explicit_sources": ["indiamart","ambitionbox"] },
  "target_rows": 200,
  "freshness": "continuous",   // one_shot | continuous | scheduled
  "dedup_policy": "canonical", // canonical | exact | off
  "budget": { "max_provider_cost_usd": 0, "max_runtime_s": 600 }
}
```

Execution model — the **fan-out → resolve → materialize** loop (reuses `JobRunner` strategies under the hood, but now driven by and writing into the workbook):

```
ICP ──► Query Planner (LLM expands ICP → per-source query templates)
     ──► Fan-out across selected sources  (existing source_registry.search_source)
     ──► Candidate stream  (raw company/person records, messy, duplicated)
     ──► Entity Resolution (Pillar 4) → canonical entities
     ──► Materialize as WorkbookRow  (with provenance + corroboration count)
     ──► WebSocket push: rows appear live as the engine finds them
```

A workbook can have **multiple source columns** (e.g. one for Maps, one for LinkedIn-via-DDG, one for job boards) feeding the same row set — they are *unioned and entity-resolved*, not concatenated.

### Pillar 2 — Living Workbooks (standing query, not a one-shot run)

A workbook with `freshness: continuous|scheduled` becomes a **subscription to a slice of the market**.

- **No scheduler exists today** — but `queue_service` already has `Job.next_run_at` + retry. Implement recurrence as a self-re-enqueuing `refresh_workbook` job (on completion, insert the next `Job` with `next_run_at = now + interval`). No new infra (APScheduler/Celery) needed.
- New entities that match the ICP and didn't exist before are appended as new rows and auto-enriched (the entity graph dedups against existing rows so refresh ≠ duplicate).
- Existing rows are **re-checked for staleness** (email bounce, website down, funding round) and re-enriched when stale — **per-field**, using the `freshness` map, not whole-row.
- The **signal monitor** becomes a *source trigger*. **Today signals are inert** — manual-scan-only and wired to neither score nor action (`monitor.py:185`). P3 closes both loops: scan on schedule (via the recurring job) → update `score` → trigger the matching workbook (re-source decision-makers, fire output column).

```
WorkbookRefreshPolicy:
  interval: "daily" | "hourly" | cron-expr
  on_signal: ["hiring_spike", "funding_round", "tech_change", "new_review"]
  staleness_ttl_days: { email: 30, funding: 90, decision_makers: 60 }
  on_new_match: "append + enrich"
  on_stale: "re-run waterfall for stale fields only"
```

This is the feature Clay's "Signals" gestures at but cannot fully deliver, because **Clay cannot source the new rows** — it can only react on rows you already imported.

### Pillar 3 — Agentic Enrichment (beyond static waterfall)

Static waterfall = fixed provider order. **Agentic column** = an LLM planner picks the provider chain _per row_, adapts on failure, and can drive a real browser for JS-heavy / form-gated sites.

New column type `agent`:

```jsonc
{
  "id": "find_ceo_email",
  "type": "agent",
  "goal": "Find the verified email of the CEO/founder",
  "tools": ["crosslinked","hunter_io","pattern_gen","mailscout_verify","browser_use","ddg_search"],
  "policy": {
     "stop_when": "verified email found",
     "max_steps": 6,
     "max_cost_usd": 0.05,
     "prefer": "free_first"   // free_first | fastest | highest_confidence
  }
}
```

The agent:
1. Plans: "no website → first find domain via DDG; then CrossLinked for the name; then pattern-gen; then SMTP verify."
2. Executes step-by-step, observing each result. Each step is a call to an existing provider in the registry — the agent reuses the same `provider.enrich(lead)` interface, it just *chooses* the order dynamically instead of `DEFAULT_WATERFALLS`.
3. Falls back to **browser-use** when HTML scraping fails on JS/SPA sites or contact forms. (A browser-job queue already exists — `bull:browser-jobs` was observed in Redis — so the runtime is partly there.)
4. Self-heals: if a provider 429s or a selector breaks, it reroutes instead of failing the cell. (Today there is **no** cooldown/retry on providers except a mailscout SMTP cache — the agent's reroute logic and Pillar 5's `cooldown_until` cover this.)
5. Emits a **reasoning trace** into provenance so the user can audit *why* a value was chosen.

Static `waterfall` stays as the cheap/deterministic default; `agent` is the premium path for hard rows. Both run through the P-1 substrate (each agent step is bounded by the cell's `max_steps`/`max_cost_usd`).

### Pillar 4 — Canonical Entity Graph (the connective tissue)

91 sources return the same company under different names, URLs, and granularities. Without resolution, a workbook is duplicate soup. This is the **hardest and most defensible** pillar — **and 80% of the matching engine already exists, unwired.**

> **Build-on, not greenfield.** `services/dedup.py:272` already implements `LeadDeduplicator`: Union-Find clustering over blocking keys (name-prefix, domain, phone-last-7, city+first-word) → pairwise scoring with field weights (company .35 Jaro-Winkler / domain .30 exact-then-fuzzy / phone .15 / email .10 / city .10) → merge waterfall (pick most-complete master, fill empties, aggregate `source` with `\|`). The live pipeline **never calls it** — `pipeline.py:15` runs only naive `deduplicate_leads(name, threshold=85)`. P1 = (a) route the live path through `LeadDeduplicator`, (b) **persist clusters as `CompanyEntity`/`PersonEntity`** instead of one-shot in-memory merge.

- **Canonical entities**: `CompanyEntity`/`PersonEntity` = a *persisted* dedup cluster. Identity keys reuse the existing blocking keys (normalized domain > name+geo > phone > social).
- **Matching**: reuse `LeadDeduplicator`'s scorer as-is; only add a persistence layer + an ambiguous-pair review queue (scores in a configurable grey band) and a `split` to undo bad merges.
- **Corroboration score** (net-new value): today the merge flattens sources into `a\|b\|c` and loses count/agreement. Persist `corroboration_count` and `source_agreement` (do N sources agree on the phone?). An entity in IndiaMart + AmbitionBox + Maps + LinkedIn outranks a single-directory hit. Feeds `scoring.py` as a new signal.
- **Field-level provenance**: every field on the canonical entity records `{value, source, provider, confidence, observed_at, corroborated_by:[...]}`. Conflicts resolved by confidence × freshness × source-trust.
- The workbook row binds to a `canonical_entity_id`; the visible cell value is the **winning** value, but the user can expand a cell to see all candidates and override.

```python
class CompanyEntity(Base):
    id            # canonical id
    primary_domain
    canonical_name
    identity_keys   # JSON: {domains:[], phones:[], socials:[], name_variants:[]}
    fields          # JSON: {field: [{value, source, provider, confidence, observed_at, corroborated_by}]}
    corroboration_count
    first_seen / last_seen
```

### Pillar 5 — Cost-Aware Waterfall Planner

With ~30 providers (free scrapers + BYOK paid APIs), naive ordering wastes money and time. This pillar is **genuinely greenfield** — intel confirms the `EnrichmentProvider` ABC has **no** `cost_per_lookup`/`free_tier_limit`, there is **no** cooldown/rate-limit handling, **no** provider stats, and **no** budget enforcement anywhere. Chains are static (`DEFAULT_WATERFALLS`). The planner orders by **expected yield ÷ cost**, learned from history.

- Extend the ABC: `requires_api_key: bool`, `free_tier_limit: int`, `cost_per_lookup: float`.
- Track per-provider, per-field in `ProviderStat`: hit-rate, avg confidence (already on `EnrichmentResult`), avg latency (already `duration_ms`), $ cost, `cooldown_until`.
- Order: free + high-yield first; paid APIs only when free chain misses; skip providers in cooldown. (Replaces today's hardcoded free-first/BYOK-last order with a *learned* one.)
- Respect the column/workbook `budget` ceiling — stop when budget hit, log what was skipped (no silent truncation).
- Surfaced as a **"why this order"** explainer + a per-workbook cost meter.

---

## 3. Data Model Changes

New / changed tables (additive — existing `Workbook`/`WorkbookRow` preserved):

```python
# NEW — canonical entity graph
CompanyEntity(id, primary_domain, canonical_name, identity_keys JSON,
              fields JSON, corroboration_count, first_seen, last_seen)
PersonEntity(id, full_name, identity_keys JSON, company_entity_id,
             fields JSON, corroboration_count, first_seen, last_seen)
EntityMergeLog(id, kept_id, merged_id, reason, score, created_at)

# CHANGED — WorkbookRow gains canonical binding + provenance
WorkbookRow:
  + canonical_entity_id  (FK, nullable)
  + provenance JSON       # {field: {source, provider, confidence, observed_at}}
  + corroboration_count
  + freshness JSON        # {field: last_verified_at}

# CHANGED — Workbook gains living-refresh config
Workbook:
  + refresh_policy JSON    # interval / on_signal / staleness_ttl
  + source_columns JSON    # ICP-driven source block configs
  + budget JSON            # cost ceiling + spend-to-date

# NEW — provider performance ledger (feeds cost-aware planner)
ProviderStat(provider, field, hits, misses, avg_confidence,
             avg_latency_ms, cost_usd, cooldown_until)
```

New column types added to `COLUMN_TYPES`: **`source`** (emits rows) and **`agent`** (autonomous enrichment).

---

## 4. API Surface (additions to `routers/workbooks.py`)

```python
# Source blocks
POST   /api/workbooks/{id}/sources                 # add a source column (ICP config)
POST   /api/workbooks/{id}/sources/{src}/run       # materialize rows now (returns job_id)
GET    /api/workbooks/{id}/sources/{src}/preview    # dry-run: estimate row count + sample, no write

# Living workbook
PUT    /api/workbooks/{id}/refresh-policy           # set interval / signals / staleness TTLs
POST   /api/workbooks/{id}/refresh                  # manual refresh (new matches + stale re-enrich)
GET    /api/workbooks/{id}/activity                 # feed: rows added, signals fired, re-enrichments

# Agentic enrichment
POST   /api/workbooks/{id}/columns/{col}/run        # already exists; agent type routes to planner
GET    /api/workbooks/{id}/rows/{row}/cells/{col}/trace   # reasoning + provenance trace

# Entity graph
GET    /api/entities/company/{eid}                  # canonical record + all source candidates
POST   /api/entities/merge                          # manual merge of ambiguous pair
POST   /api/entities/split                          # undo a bad merge

# Cost / planner
GET    /api/workbooks/{id}/cost                     # spend-to-date, per-provider breakdown
GET    /api/providers/stats                         # yield/cost/latency ledger
```

WebSocket events extend the existing channel: `row_added` (live sourcing), `entity_merged`, `cell_trace`, `budget_warning`, `signal_fired`.

---

## 5. Chat-Native Authoring (the UX moat)

The chat already creates jobs and merges to workbook. V2 closes the loop so chat **authors the engine**, not just one run:

- "Build a living workbook of SaaS CTOs in SF that refreshes weekly" → creates workbook + source column + `refresh_policy.interval=weekly` + agent email column.
- "Only keep companies seen on at least 2 sources" → sets `corroboration_count >= 2` filter.
- "When any of these start hiring, find the hiring manager and draft an intro" → wires signal trigger → agent column → output column.

This maps to existing tool-calling infra (`copilotkit.py`); the new tools are `create_source_column`, `set_refresh_policy`, `add_agent_column`, `add_signal_trigger`.

---

## 6. Phased Delivery

| Phase | Scope | Unlocks | Est. |
|---|---|---|---|
| **P-1 — Execution substrate** ✅ **SHIPPED** | `queue_service` `run_workbook` handler, bounded-concurrency batched runner, status state machine + reaper recovery, Redis publisher wired | **Unblocks everything** — enrichment actually completes | done |
| **P0 — Source Column** ✅ **SHIPPED** | `source` column type, ICP→query, fan-out via `JobRunner`, dedup vs existing rows, live `row_added` publish, `/sources` + `/run` + `/preview` endpoints | Sourcing *inside* the workbook | done |
| **P1 — Entity Resolution** | Wire `LeadDeduplicator` into live path; persist `CompanyEntity` graph; corroboration score; candidate expansion + split | No more duplicate soup; trust signal | 3–4 d |
| **P2 — Provenance + Cost** | per-field provenance, extend ABC (`cost`/`free_tier`), `ProviderStat` ledger, cost-aware ordering, budget meter | Auditable + cheap | 2–3 d |
| **P3 — Living Workbooks** | `refresh_policy`, recurring `queue_service` job, per-field staleness re-enrich, signal→score→action wiring | Standing market subscription | 3–4 d |
| **P4 — Agentic Column** | `agent` column, per-row planner, browser-use fallback, reasoning trace | Hard rows get solved | 4–5 d |
| **P5 — Chat authoring + Functions** | chat tools to author the engine; package source+enrich as reusable Functions/templates | Distribution + reuse | 2–3 d |

**P-1 is non-negotiable and ships first** — without it, nothing the user runs completes (workbooks stick in `running`). Then P0 (headline) → P1 (moat) → P2. P1 is now smaller than first estimated because `LeadDeduplicator` already exists.

### Implementation status (as built)

**P-1 ✅** — `services/workbook/enrichment.py::run_workbook_enrichment` (bounded-concurrency batched cells, each in its own Session; pause-aware; finalizes status in `finally` so a run can never stick in `running`) + `handle_run_workbook` registered on `queue_service` in `main.py`. `routers/workbooks.py::run_workbook` now enqueues one durable job and returns immediately (the dead BullMQ→Redis-list→serial-inline path is removed). Verified with a 4-case functional test: completes on success, **completes (not stuck) when a cell raises**, honors pause, handles empty.

**P0 ✅** — `services/workbook/source_engine.py` (`build_query`, `materialize_source`, `handle_source_workbook`, `preview_source`); `source` added to `COLUMN_TYPES`; endpoints `POST /sources`, `POST /sources/{col}/run`, `GET /sources/{col}/preview`; `source_workbook` handler registered. Reuses `JobRunner.submit` for sourcing, snapshots leads into `WorkbookRow` with name+domain dedup vs existing **and** in-batch, publishes `row_added`. Verified: query build, real-registry preview, dedup/insert counts, and **idempotent re-run (added=0)**.

> Dedup in P0 is the naive normalized-key version by design — **P1 replaces it with the existing `LeadDeduplicator`** (Jaro-Winkler + Union-Find) and persists clusters as `CompanyEntity`.

---

## 7. Acceptance Criteria (per pillar)

- **Execution substrate (P-1)**: A 500-row × 5-col workbook `/run` completes to `status=complete` without blocking the HTTP request; cells process concurrently; a killed/timed-out run is auto-recovered by the reaper (never stuck `running`); cell completions stream over the WS in real time.
- **Source Column**: From an empty workbook, an ICP string materializes ≥N entity-resolved rows with live WS updates; re-running does not duplicate existing rows.
- **Entity Resolution**: Same company from ≥3 sources collapses to one row with `corroboration_count≥3`; precision ≥0.95 / recall ≥0.85 on a labeled fixture set (use `data/pipeline_eval/` verticals as ground truth).
- **Provenance**: Every populated cell exposes `{source, provider, confidence, observed_at}`; conflicts resolve deterministically and are explainable.
- **Cost**: A workbook with a `$0` budget never calls a paid BYOK provider; budget ceiling is never exceeded; skipped providers are logged.
- **Living**: A scheduled workbook appends only genuinely-new matches on refresh; stale-only fields (not whole rows) are re-enriched.
- **Agent**: On a fixture of "hard" rows (no website, SPA site) the agent column beats static waterfall hit-rate by ≥20% within its step/cost budget.

---

## 8. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Entity resolution false-merges → wrong data | Conservative threshold + human-review queue for ambiguous pairs + `split` endpoint + `EntityMergeLog` audit |
| Continuous sourcing → rate-limit bans / proxy drain | Reuse cooldown-based proxy benchmarking (lesson from blueprint anti-patterns); per-source rate limits; scheduled (not hot-loop) refresh |
| Agent column cost/latency runaway | Hard `max_steps` + `max_cost_usd` per cell; free-first policy; budget ceiling enforced at planner |
| Browser-use brittleness | Used only as fallback after cheap paths fail; sandboxed; timeout-bounded |
| Scope creep vs launch timeline | P0 (Source Column) ships standalone and is the demo; later pillars are additive |
| Compliance (scraping / PII) | Respect robots/ToS posture already in scrapers; provenance enables data-subject deletion; keep self-hosted = customer owns the data |
| **Worktree ≠ running app** | The live app runs from `/Users/user4/Desktop/yupcha/lead-data` against `data/data.db`, not this `workbook-v2` worktree. Confirm which tree ships P-1; don't validate fixes only in the worktree. |
| **BullMQ assumed but absent** | `bullmq` isn't installed and no worker runs; current code silently degrades to a dead Redis list. P-1 makes `queue_service` the backbone so BullMQ is optional, never load-bearing. |

---

## 9. Why This Wins

1. **Sourcing-in-the-workbook** is a category the incumbents structurally cannot ship — Clay's revenue is the credit-metered enrichment marketplace; native free sourcing cannibalizes it.
2. **Entity resolution across 91 sources** turns a pile of scrapers into a *corroborated truth layer* — the thing that actually makes B2B data trustworthy, which no single-DB vendor (Apollo) can match.
3. **Living workbooks** convert a one-time tool into a standing subscription — the basis of retention and the Cloud/Agency revenue lines in the master blueprint.
4. **Self-hosted + open** means the customer owns the corroborated entity graph — an asset that compounds, not a credit balance that depletes.

> Build order: **Source Column (P0) → Entity Graph (P1) → Provenance/Cost (P2)**. That trio alone is a demoable, defensible "industry-first lead source engine." The rest compounds on top.
