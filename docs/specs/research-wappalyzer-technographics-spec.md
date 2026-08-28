<!-- Auto-generated research-bet spec (2026-06-26). Effort: medium. -->

## Build-ready spec — Full Wappalyzer-fork technographics

### TL;DR / honest framing
The "full Wappalyzer-fork fingerprint set" is **already 70% shipped** and the framing in this bet is stale. The full open fingerprint DB is bundled (`apps/api/services/leadgen/enrichment/data/tech_fingerprints.json`, **5227 entries, 477 KB**) and a working detection engine reads it (`tech_stack_provider.py`). The remaining, genuinely-missing work is **hardening + wiring + compliance**, not building an engine from scratch. This spec scopes that delta and bakes in the adversarial passes (license provenance, SSRF, politeness/cost, tenancy, signal wiring).

---

### Goal
Turn the existing best-effort website tech detector into a production technographics feature: license-clean and reproducible fingerprint DB, safe + polite homepage fetch, cached, structured persistence, fed into both scoring and the intent/watch signal feed, behind a default-OFF workspace flag.

---

### Grounded current state (file:line)

**Path A — website fingerprinting (the Wappalyzer path):**
- `apps/api/services/leadgen/enrichment/providers/tech_stack_provider.py` — `TechStackProvider`.
  - `:25` `_DB_PATH` points at the bundled full DB; `:111-115` `_load_db()`; `:118-145` `_build_compiled()` merges full DB + a curated `FINGERPRINTS` subset (`:33-108`), curated wins on category, inherits `implies`.
  - `:165-227` `detect_tech_from_response(headers, html, cookies)` — matches headers/html/meta/cookies, resolves `implies` transitively (e.g. WooCommerce⇒WordPress⇒PHP).
  - `:230-330` `enrich()` — raw `httpx.AsyncClient(timeout=15, follow_redirects=True, verify=False)` (`:252-259`), spoofed Chrome UA (`:255`), caps HTML at 200 KB (`:264`), emits `fields={"technologies": "<comma list>"}` (`:303`).
- `apps/api/services/leadgen/enrichment/data/tech_fingerprints.json` — 5227 entries (first entry `T-Soft`, webappanalyzer-shaped). **No LICENSE/NOTICE/provenance file beside it; no regen script in repo.**
- Registration: **workbook only** — `apps/api/services/workbook/providers.py:158-160` registers `TechStackProvider` unconditionally; `apps/api/services/workbook/enrichment.py:92` maps capability `"technologies": ["tech_stack"]`. Capability registry: `enrichment/declarative/registry.py`.

**Path B — job-text technographics (already feeds signals):**
- `apps/api/services/leadgen/enrichment/providers/job_tech_intent.py` — curated `TECH_DICTIONARY` + intent classifier; `analyze_job_text()` (`:280+`) emits `technologies` / `tech_adoption_signal{tech,category,intent}` / `hiring_velocity`. Callers: `jobspy_signals.py`, `ats_hiring.py`.
- Signal wiring: `apps/api/services/poller/sources.py:175-260` `fetch_hiring_and_tech()` diffs `tech_adoption_signal` against `watch.cursor["known_tech"]` and emits `new_tech_adopted` events (intent-tier-aware, dedup once per tier). `poller/engine.py:503,516` gate on `wants("new_tech_adopted")`. **This is jobspy text only — Path A (website) feeds nothing.**

**Persistence / scoring / signal types:**
- `models.py:41` `technologies: str` (flat comma string); `orm_models.py:85` `technologies = Column(Text)`; `db.py:223` SQLite DDL. RLS-scoped via the `leads` table.
- In the core leadgen run, `technologies` comes **only from AI extraction** — `job_runner.py:1048` `technologies=ai_data.get("technologies")` (`ai_stages.py:104`). `TechStackProvider` does **not** run in the sourcing waterfall.
- Signal taxonomy defines `tech_change` (`signals/monitor.py:38`) but **it is never emitted** (only referenced as a refresh-trigger enum in `copilotkit.py:658`, `workbook/refresh.py:222`). Watch event type is `new_tech_adopted` (`routers/watches.py:43-45`).
- Shared enrichment cache exists but tech_stack does **not** use it: `cache.py:67` `canonical_key(field, lead)` (company fields key on domain), `:122/:140` `EnrichmentCache.get/set`, `:171` `get_cache()`.
- Flag pattern: `core/config.py` pydantic settings, e.g. `COMPANY_SIZE_HEURISTIC_ENABLED:248`, `INTENT_POLLER_ENABLED:173`. Workbook does conditional chain-append gated on a flag (`workbook/enrichment.py:100-111`).
- Roadmap framing: `docs/research/improvement-roadmap-2026-06-25.md:38` expects "~250 fingerprints" — the bundled 5227 contradicts this and is itself the provenance red flag (where did 5227 come from?).

---

### Design

**1. License & reproducibility (do first — gating).**
Pin the source to **`enthec/webappanalyzer`** (MIT/GPL-clean continuation of the last-open Wappalyzer dataset) or **`tunetheweb/wappalyzer`**. Add `data/tech_fingerprints.NOTICE` recording: upstream repo, exact commit SHA, license (MIT), retrieval date, and the transform applied. Add a reproducible regen script `scripts/build_tech_fingerprints.py` that downloads the pinned `technologies/*.json` + `categories.json`, compiles to the provider's flat format, and writes both `tech_fingerprints.json` and the NOTICE. This makes the existing mystery 5227-entry blob auditable and re-buildable. Categories must be mapped from upstream IDs to readable labels (current curated labels are better than the raw `cat` strings).

**2. Safe + polite fetch.**
Replace the raw httpx block (`tech_stack_provider.py:252-259`) with a shared `_safe_fetch(url)`:
- **SSRF guard**: resolve host, reject private/loopback/link-local/metadata IPs (`ipaddress.ip_address(...).is_private/.is_loopback/.is_link_local` + block `169.254.169.254`); re-check after each redirect (cap redirects at 3). `lead.website` is user/tenant-controlled so this is a real confused-deputy vector today.
- **Drop `verify=False`** by default (it disables TLS verification on attacker-influenceable URLs); add `TECH_STACK_INSECURE_TLS` escape hatch (default False).
- **Politeness**: honest identifying UA (`Yupcha-TechDetect/1.0 (+https://yupcha.com/bot)`), optional `robots.txt` respect (flag, default respect), per-domain rate limit / in-flight dedup, `timeout=10`, HTML cap stays 200 KB.
- **Fetch budget**: homepage-only by default (no crawl). One GET per domain per run.

**3. Caching.**
Wrap detection in the shared cache keyed by domain. Use `canonical_key("technologies", lead)` (`cache.py:67`, company-field branch keys on normalized domain) → `get_cache().get/set` with a 90-day TTL (tech changes slowly; matches `company_size` TTL). Cache the **structured** result (below) as JSON. This kills duplicate fetches across leads sharing a domain and across re-runs — the central cost/politeness lever.

**4. Structured persistence + scoring.**
Keep `technologies` (comma string) for back-compat (workbook cells require flat scalars — `provider.py` data contract). **Add** a JSON field `technographics` = `[{name, category, source:"website", confidence}]` stored like `hiring_signals`/`decision_makers` (JSON-in-Text column). Migration adds the column to `leads` (alembic, current head `a7b8c9d0e1f2`) and the SQLite DDL (`db.py`). Scoring (`leadgen/scoring.py`) optionally rewards ICP-relevant categories (CRM/MarTech/eCommerce) — additive, behind the same flag.

**5. Signal/intent wiring (close the gap).**
Make website technographics emit signals like the jobspy path. Add a website-tech diff in the poller analogous to `fetch_hiring_and_tech` (`sources.py:175-260`): store `cursor["known_web_tech"]`, and on a newly-seen technology emit a `new_tech_adopted` (source `"website"`) or the dormant `tech_change` type. Reuse `keys.normalize_tech` for dedup. Default-OFF and only for watches that opted into tech.

**6. Default-OFF rollout.**
The website-fetch path makes outbound calls per lead (cost/politeness/legal surface), so gate it. Add `TECH_STACK_WEBSITE_FETCH_ENABLED: bool = False`. The job-text Path B stays on (zero new network cost). With the flag off, behavior is byte-identical to today.

---

### File-by-file changes
- `apps/api/services/leadgen/enrichment/providers/tech_stack_provider.py` — extract `_safe_fetch` (SSRF + TLS + politeness); integrate `get_cache`; return structured `technographics` alongside `technologies`; gate the fetch on the new flag (return graceful "disabled" `EnrichmentResult` when off).
- `apps/api/services/leadgen/enrichment/data/tech_fingerprints.NOTICE` — **new**: provenance + license.
- `scripts/build_tech_fingerprints.py` — **new**: reproducible regen from pinned upstream commit.
- `apps/api/core/config.py` — add `TECH_STACK_WEBSITE_FETCH_ENABLED`, `TECH_STACK_RESPECT_ROBOTS=True`, `TECH_STACK_INSECURE_TLS=False`.
- `apps/api/services/workbook/providers.py:158-160` — keep registration (workbook column is opt-in per cell); the network gate lives in the provider, not registration.
- `apps/api/services/leadgen/models.py` / `orm_models.py` / `db.py` — add `technographics: str = ""` (JSON-in-Text).
- `alembic/versions/<new>.py` — add `technographics` column to `leads` (nullable default `''`), branch off `a7b8c9d0e1f2`.
- `apps/api/services/poller/sources.py` — add website-tech diff → `new_tech_adopted`/`tech_change` (flag + opt-in).
- `apps/api/services/leadgen/scoring.py` — optional additive category scoring (flag).
- `apps/api/services/signals/monitor.py` — wire the `tech_change` type to actually emit if used.

---

### Data / persistence + tenancy/security
- `technologies` and `technographics` live on the RLS-scoped `leads` table → per-workspace isolation is inherited; no new tenant boundary. Verify the alembic migration adds the column **before** any RLS policy re-grant so safe-role still sees it (`PG_RLS_REQUIRE_SAFE_ROLE`).
- The shared `EnrichmentCache` (SQLite, `cache.py`) is **global, not tenant-scoped** — acceptable because cached data is public website fingerprints keyed by domain (not PII), consistent with existing email/company-size caching. Document this explicitly; do not cache anything lead-private under it.
- SSRF is the headline security item: `lead.website` is tenant-controlled and today flows into an unguarded fetch with TLS verification off — confused-deputy / internal-network probe risk on self-host. SSRF guard + drop `verify=False` are mandatory, not optional.

### Infra/deploy
- No new services. Bundled JSON ships in the image (`docker-compose.yml` self-host unaffected; +477 KB, already present). The regen script is dev-only (CI optional).

### License/compliance
- **Decision-gating.** Must (a) confirm the bundled 5227-entry blob's provenance is an MIT/permissive fork (NOT a scrape of post-2023 commercial Wappalyzer), (b) add NOTICE + retain upstream LICENSE text, (c) make it reproducible. If provenance can't be established, regenerate from `enthec/webappanalyzer` at a pinned commit and replace the blob.

### Feature-flag / rollout (default-OFF)
- `TECH_STACK_WEBSITE_FETCH_ENABLED=False` (master, network path). Job-text technographics unaffected. Roll out: internal workspace → opt-in beta → consider default-on for self-host (where cost/politeness is the operator's own) while keeping cloud default-off.

---

### Test plan
- **Unit (no PG, no network):** `detect_tech_from_response` golden tests for headers/html/meta/cookies + `implies` resolution (extend `tests/test_job_tech_intent.py` sibling `test_tech_stack.py`). SSRF guard: assert private/loopback/metadata IPs and post-redirect rebinding are rejected. Flag-off: provider returns disabled result, zero `httpx` calls (assert via mock).
- **Cache:** second `enrich()` for same domain hits cache, issues no fetch (mock).
- **License:** test that `tech_fingerprints.json` parses, every entry compiles (no `re.error`), and a NOTICE file exists.
- **PG-gated (`PYTHONPATH=. uv run --group dev python -m pytest`, live throwaway PG — never touch `yupcha` db):** migration up/down for `technographics`; RLS read isolation (workspace A cannot read B's `technographics`); poller `new_tech_adopted` diff emits once per newly-seen tech and dedups on cursor.
- **Regression:** with flag off, sourcing run output is byte-identical (snapshot).

---

### Numbered acceptance criteria
1. `tech_fingerprints.json` has a co-located NOTICE recording upstream repo + pinned commit + MIT/permissive license, regenerable via `scripts/build_tech_fingerprints.py`.
2. Website fetch rejects private/loopback/link-local/metadata IPs (incl. post-redirect) and performs TLS verification by default.
3. Fetch is homepage-only, ≤1 GET/domain/run, honest identifying UA, respects robots by default, and is cached (≥2nd lookup for same domain issues no network call).
4. Detection persists both legacy `technologies` (comma string) and structured `technographics` JSON `[{name,category,source,confidence}]`.
5. `TECH_STACK_WEBSITE_FETCH_ENABLED=False` ⇒ no outbound fetches and byte-identical sourcing output; `=True` ⇒ detection runs.
6. Newly-detected website technology emits a `new_tech_adopted`/`tech_change` signal once (deduped via watch cursor), workspace-scoped.
7. `technographics` is RLS-isolated per workspace (PG test proves cross-tenant read fails).
8. Full unit + PG-gated suite green.

### Out of scope
Multi-page crawling; JS-rendered/headless detection; version extraction; BuiltWith-parity breadth claims; a public technographics API; backfilling website tech onto historical leads; replacing the job-text Path B.
