<!-- Auto-generated sourcing P2/P3 spec (2026-06-26). Effort: medium. -->

## Mini-spec: Company-size heuristic signals

### Goal
Infer a company's employee band (and, secondarily, revenue band) from signals Yupcha **already collects** during sourcing/enrichment — job-posting volume, registry data (SEC Form D / Companies House), technographics, site footprint, company age — and expose it as a populated `company_size` lead field plus a filter. **No new paid APIs**: this is a pure, in-memory derivation over fields already on the `Lead`, plus a band-normalizer that fixes a latent scoring bug.

---

### Grounded current state

**The field already exists end-to-end** — only its *population* is weak/inconsistent:
- `Lead.company_size: str` ("1-50"/"51-200"/"201-500"/"500+"), `employee_count_exact: int`, `revenue_range: str`, `hiring_signals: str` (JSON), `technologies: str` — `apps/api/services/leadgen/models.py:35-42,54`.
- Persisted in Postgres (`PgLeadStore`) at `apps/api/services/leadgen/orm_models.py:76-96` and legacy SQLite at `apps/api/services/leadgen/db.py:81,219-233`.

**Scoring already consumes it (and has a latent bug):** `scoring.py:45-46` adds `SCORING_WEIGHTS["company_size_large"]` (=15, `config.py:86`) **only** when `company_size in ("51-200","201-500","500+")` — an **exact string match**. Anything outside that exact vocabulary (a raw integer, "1-50", "10-50", "5000") scores zero.

**Size is populated today by ad-hoc, lossy paths:**
- LinkedIn regex bucketer — `job_runner.py:1285-1296`.
- `_extract_size(body)` regex bucketer — `job_runner.py` (used at `:1390,1505`).
- Waterfall chain `DEFAULT_WATERFALLS["company_size"] = ["deep_scraper","website_scraper","company_intel","wikidata","people_data_labs"]` — `apps/api/services/workbook/enrichment.py:77`. These are scrape/paid providers that frequently fail (see `docs/research/data-source-test-report.md`).
- Inert paid manifest `company_size/leadmagic_company.yaml` — maps `$.employee_count` (an **integer**) straight into `company_size`, which then fails the scoring string match.

**Raw signals that are ALREADY on the lead (the heuristic's inputs — zero new network):**
- `hiring_signals` JSON: `total_jobs`, `growth_signal` ("hiring"→"hypergrowth"), `technologies`, `tech_adoption_signal` — built by `jobspy_signals.py:45-122` and `ats_hiring`.
- SEC Form D `funding_amount` (`sec_edgar.py:366-368`) and `funding_stage_signal`.
- Companies House `decision_makers` (director/PSC count), `company_status`, `incorporation_date`/`founded_year` — `companies_house.py:238-326`.
- `technologies` count (technographic footprint), `founded_year` (age), `description` length, `decision_makers` count (site/registry footprint), JSON-LD firmographics (`jsonld_firmographics.py`).

**Provider + registry plumbing to reuse:**
- `EnrichmentProvider` ABC + data contract (flat scalars; JSON only in designated fields) — `provider.py:65-107`.
- `WaterfallEnricher.apply_results` already **only fills empty cells** (`provider.py:253-259`) — so a low-priority heuristic cannot clobber a real value.
- Auto-registration via `register_provider()` in `workbook/providers.py:21-24,51-55`; capability resolution in `declarative/registry.py:resolve`.

**Filter plumbing:**
- Workbook filter compiler **already supports** `company_size` exact match — `workbooks.py:87-89`.
- `PgLeadStore.get_leads` (`store.py:229-277`) does **not** expose a `company_size` filter; `/leads` route (`leads.py:48-79`) doesn't either.
- Tenancy: `PgLeadStore` is RLS-scoped by `workspace_id` (`store.py:144,248`); the heuristic reads only the in-memory `Lead`, so it introduces no cross-tenant surface.

---

### Design

**1. Canonical band vocabulary (keep the existing 4 to avoid destabilizing scoring).**
`SIZE_BANDS = ["1-50", "51-200", "201-500", "500+"]` — identical to the strings `scoring.py:45` already checks. No change to scoring's membership set or weights, so existing scores are unaffected.

**2. `normalize_band(value) -> str`** (new pure fn): coerces any incoming `company_size` — raw int, "10-50", "5,000", "501-1000", "Series ranges" — into one canonical band, else "". This **fixes the latent bug** where LeadMagic/JSON-LD integers never scored. Called (a) in `scoring.py` before the membership check, and (b) by the heuristic provider on its own output.

**3. `infer_company_size(lead) -> SizeEstimate`** (new pure fn, zero network). Deterministic, ordered by signal trust:
- **Exact wins:** if `employee_count_exact > 0` → band from count, `basis="exact"`, confidence 0.95.
- **Hiring volume** (from `hiring_signals.total_jobs` / `growth_signal`): heuristic open-roles→headcount multiplier (e.g. `hypergrowth`/10+ jobs ⇒ ≥201-500; `rapid_growth`/5-9 ⇒ 51-200; `growing`/2-4 ⇒ 51-200 lean; `hiring`/1 ⇒ 1-50).
- **Registry/funding:** SEC `funding_amount` thresholds (e.g. >$10M ⇒ ≥51-200), Companies House director+PSC count and company age (`founded_year`) nudge the band up.
- **Footprint:** `technologies` count, `decision_makers` count, `description` length as weak tie-breakers.
- Combine via a small weighted vote; emit `(band, confidence∈[0.3,0.6], basis="heuristic:<signals>")`. Confidence is deliberately **below** real providers so it loses the waterfall when a real one fires.

**4. `CompanySizeHeuristicProvider`** (capability `["company_size"]`, `requires_api_key=False`, `default_confidence≈0.4`): wraps `infer_company_size`, returns `{"company_size": band, "company_size_basis": basis}`. Registered **last** in the `company_size` waterfall so it only fills gaps the scrape/paid providers leave.

**5. Provenance field** `company_size_basis` so the UI/scoring can distinguish "exact" vs "estimated".

**Data flow:** existing scrapers/enrichers populate `hiring_signals`, `funding_amount`, `technologies`, `founded_year`, etc. → `company_size` waterfall runs `deep_scraper…people_data_labs` (real data) → falls through to `company_size_heuristic` (derives from the above) → `apply_results` writes only if empty (`provider.py:258`) → `score_and_update_db` re-scores using `normalize_band`.

---

### File-by-file changes

**New**
- `apps/api/services/leadgen/enrichment/size_heuristic.py` — `SIZE_BANDS`, `normalize_band()`, `band_from_count()`, `infer_company_size()`, `revenue_band_from_funding()` (secondary, off the scoring path). Pure stdlib, no I/O.
- `apps/api/services/leadgen/enrichment/providers/company_size_heuristic.py` — `CompanySizeHeuristicProvider`.
- `apps/api/services/leadgen/tests/test_size_heuristic.py` — unit tests.

**Edited**
- `apps/api/services/leadgen/models.py:42` — add `company_size_basis: str = ""` after `funding_stage`.
- `apps/api/services/leadgen/orm_models.py` (~:77) — add `company_size_basis = Column(String, default="")`.
- `apps/api/services/leadgen/db.py` (~:219) — add `"company_size_basis": "TEXT DEFAULT ''"` to the legacy auto-migrate map.
- Alembic migration (head per memory `b2c3d4e5f6a7`) — add nullable `company_size_basis` to `leads`.
- `apps/api/services/leadgen/scoring.py:45-46` — wrap with `normalize_band(lead.company_size)` before membership test (bug fix; behind no flag — strictly widens correctness, never lowers an existing valid band).
- `apps/api/services/workbook/enrichment.py:77` — append `"company_size_heuristic"` to the `company_size` chain (last).
- `apps/api/services/workbook/providers.py` (~:51 `_init_providers`) — `register_provider(CompanySizeHeuristicProvider())` guarded by feature flag.
- `apps/api/services/leadgen/store.py:229-277` — add `company_size: Optional[str]=None` param + `q.filter(LeadRow.company_size == company_size)`; add to `_ALLOWED_ORDERS` only if sortable needed.
- `apps/api/routers/leads.py:48-79` — add `company_size` query param, pass through.
- (Optional UI) `apps/web` leads/workbook filter sidebar — `company_size` dropdown (filter compiler already supports it at `workbooks.py:87-89`).

---

### Data / persistence + tenancy
- One new nullable scalar column `company_size_basis`; `company_size` reuses existing storage. No new tables.
- Heuristic is a pure function over the in-memory `Lead`; it performs **no DB reads**, so it inherits RLS automatically and cannot leak across `workspace_id`.
- Optional backfill job (gated, dry-run-first): `normalize_band` existing `company_size` values + run the heuristic on rows where `company_size==''`. Workspace-scoped via `PgLeadStore`.

---

### Backward-compat / non-destabilization
- **Scoring:** band vocabulary unchanged; weight unchanged. `normalize_band` only ever maps to one of the 4 existing strings, so no lead's score can drop. The bug-fix can only newly-credit leads whose size was previously an unmatched format (net-positive, but see Decision 2 about whether estimated bands should earn full credit).
- **Waterfall:** heuristic is last + lowest-confidence; `apply_results` fills empties only (`provider.py:258`), so it never overwrites a scraped/paid value.
- **No new network/cost/memory:** zero outbound calls, O(1) per lead; DDG circuit breaker / proxy paths untouched.
- **Field/filter additive:** `company_size` and the workbook filter already exist; new store/route param defaults to `None` (no behavior change when unset).

---

### Feature flag / rollout
- Env/flag `COMPANY_SIZE_HEURISTIC_ENABLED` (default off) gating provider registration in `providers.py` and the chain append in `enrichment.py`.
- Roll out: (1) ship `normalize_band` bug-fix (always on, low-risk), (2) enable provider in staging, sample basis distribution, (3) enable per-workspace, (4) optional backfill.

---

### Test plan
- `test_size_heuristic.py`: `normalize_band` table (ints, "10-50", "5,000", "501-1000", garbage→""); `infer_company_size` for exact-count, jobs-only, funding-only, registry-only, combined, and empty (→ "" / low confidence).
- Scoring regression: lead with int `company_size=120` now scores +15 (was 0); leads with existing bands unchanged.
- Provider: `apply_results` does **not** overwrite a pre-set `company_size`; fills when empty.
- Store/route: `get_leads(company_size="51-200")` returns only matching, workspace-scoped rows; RLS parity.
- Run `PYTHONPATH=. uv run --group dev python -m pytest` (size + scoring + store suites).

---

### Acceptance criteria
1. A `company_size_heuristic` provider exists, is keyless (`requires_api_key=False`), makes **zero network calls**, and is registered for capability `company_size`.
2. `infer_company_size` returns a canonical band ∈ `{1-50,51-200,201-500,500+}` or "" and never raises on partial/empty leads.
3. When `company_size` is empty and `hiring_signals`/`funding_amount`/registry/technographic signals are present, the lead receives an estimated band with `company_size_basis` set.
4. When `company_size` already holds a real value, the heuristic never overwrites it.
5. `normalize_band` makes previously-unmatched formats (raw int, "10-50", etc.) score correctly via `scoring.py`, and no existing valid band's score decreases.
6. `PgLeadStore.get_leads(company_size=…)` and `/leads?company_size=…` filter correctly and remain workspace/RLS-scoped.
7. Provider/chain are gated by `COMPANY_SIZE_HEURISTIC_ENABLED`; with the flag off, behavior is byte-identical to today (except the always-on `normalize_band` fix).
8. New nullable `company_size_basis` column added via Alembic + legacy SQLite map; existing rows default to "".
9. Full test suite passes.

---

### Out of scope
- Revenue-band inference is delivered as a helper (`revenue_band_from_funding`) but **not** wired into scoring or surfaced as a primary filter in v1 (Decision 3).
- New finer-grained band taxonomy (6+ bands) — would require scoring + migration changes (Decision 1).
- New data sources / paid APIs / LinkedIn headcount scraping.
- ML/learned size model (this is a deterministic heuristic).
