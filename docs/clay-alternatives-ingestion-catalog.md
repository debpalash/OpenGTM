# Clay-Alternatives → Yupcha: Master Ingestion Catalog

> Deep-dive synthesis of **150 cloned repos** (149 "clay alternative" repos + the `bricks` local-first Clay clone), analyzed by 11 parallel code-reading agents (not README skims — actual source). Date: 2026-06-07.
>
> Companion to [`clay-alternatives-research.md`](clay-alternatives-research.md) (the surface inventory). This doc is the **engineering ingestion plan**: what working code/technique to take, the exact source file, and where it lands in Yupcha.
>
> Clones live in the gitignored `reference/research/clay-alternatives/` + `reference/research/bricks/`.

## How to read this

Each asset is tagged **P0** (build first — fills a real gap, high leverage), **P1** (strong add), **P2** (reference / niche). Yupcha subsystems referenced:
- **`enrichment`** = `apps/api/services/leadgen/enrichment/` (providers, email finder/verify, scrapers)
- **`planner`** = `apps/api/services/workbook/planner.py` (cost-aware waterfall ordering)
- **`provider.py`** = `apps/api/services/leadgen/enrichment/provider.py` (`WaterfallEnricher`)
- **`workbook`** = `apps/api/services/workbook/` (columns, runner, source engine)
- **`sources`** = `apps/api/sources/` (search/scrape sources)
- **`copilot`** = `apps/api/routers/copilotkit.py` + AI/research/agent columns

---

## The 10 highest-leverage ports (the "build this first" list)

These recurred across many repos and map onto concrete gaps in Yupcha's current code.

| # | Asset | Best source(s) | Yupcha target | Why it matters |
|---|-------|----------------|---------------|----------------|
| 1 | **Confidence early-exit + best-of-N** in the waterfall (stop at `confidence ≥ 0.85`, keep best, not "first non-empty") | `masteranime/enrichment-kit` `src/waterfall/index.ts` | `provider.py` `WaterfallEnricher.enrich` | Yupcha stops at first non-empty value and ignores `result.confidence`. Direct accuracy + cost win. |
| 2 | **Cross-provider canonical-key enrichment cache** (if Apollo already found this person, never call Prospeo) | `masteranime` `utils/normalize.ts` (`canonicalKey`) + `nimajnebrevilo/GTM-Engine` `db/queries/enrichment-cache.ts` (`getAnyCachedEnrichment`) | new `enrichment_cache` table consulted in `provider.py`/`planner.py` | Yupcha has only per-provider in-process cache — no cross-provider dedup. Single biggest cost lever. |
| 3 | **Output validation accept-gate** (reject role emails `info@`, placeholders, email-domain≠company-domain, sentinel strings like `"no-results-found"`) | `masteranime` `utils/validate.ts`; `VAV-Technologies/clay-clone` `isValidEmailValue`/`isUsableCompanyDomain` | `provider.py` accept-gate + `email_verify.py` | Yupcha's `has_value` check accepts garbage. Quality gate before a value is stored/charged. |
| 4 | **Normalized multi-vendor verify cascade** (8 vendors → 4 unified statuses `valid/invalid/catch_all/service_error`; "definitive stops, throw cascades") | `bricks` `validate-email/route.ts` (676 lines); `tr4m0ryp/clay-enrichment` dual port-25 + HTTP fallback; `nimajnebrevilo` Million-Verifier | `email_verify.py` → new `email_verify_cascade.py` | One status contract over all verifiers; auto-self-configures to whatever BYOK keys exist. |
| 5 | **Vendor/cost catalog + dynamic cost calc** feeding the planner (`cost = base × scaling`; search `ceil(per_page/25)`, bulk `× count`; BYOK & cache-hit free) | `sayanta-ghosh/gtm-engine` `vendor_catalog.py`+`execution/service.py`; `nurturev/gtm-engine` `billing/cost_config_service.py` | `planner.py` `PROVIDER_COST` → DB-backed catalog | Turns the cost model into config + a "BYOK vs platform key" UI; ready provider cost tables. |
| 6 | **Column-as-action type system + per-row incremental engine + dependency DAG** | `eliasstravik/rowbound` `core/{types,engine,action-deps}.ts`; `leszek-backpack/rex` `_sort_by_deps` + `run_and_wait` status machine | `workbook` column schema + runner; `planner.py` ordering | A ready taxonomy (`http/waterfall/formula/ai`), idempotent per-row runs, and topo-sort of columns by `{{row.x}}` refs. |
| 7 | **SSRF url-guard** for any user-supplied URL (blocks private/loopback/link-local + **octal/hex/decimal IP encodings** + IPv6-mapped) | `eliasstravik/rowbound` `core/url-guard.ts` | new `apps/api/core/url_guard.py`, used by HTTP columns + source engine | Yupcha lets users define HTTP columns/sources → mandatory before that ships. |
| 8 | **Ready-to-ship AI-column prompt library** (5-part ROLE/CONTEXT/TASK/FORMAT/FALLBACK; `INSUFFICIENT_DATA`/`"purple"` null sentinel; qualification tiers, opener, tech-stack) | `forma-norden/clay-claude-code-skill-pack`; `sachacoldiq/ColdIQ` `claygent-guide.md`; `mariosworkflows/GTM-Engine` skills | `ai_column.py` defaults + `copilot` prompt-authoring guidance | Drop-in production prompts; the prompt *is* the asset for AI columns. |
| 9 | **Tech-stack + ATS hiring-signal detectors** (≈60 techs / 13 categories from HTML+headers; Greenhouse/Lever/Ashby scrapers; job-velocity signal) | `rahulchhabria/local-enrichment-tool` `tech-detector.ts`+`job-scraper.ts`; `Tekipeps/CompanyEnrichment` Exa hiring-velocity | new `tech_stack` + `hiring_signals` providers in `enrichment` | Free technographic + hiring-intent columns, no BuiltWith bill. |
| 10 | **Agentic email-finder loop** (4-phase: scan→top-patterns→web-search w/ masked-email deduction→fuzz; sequential validate; 2×catch_all-stops) | `bricks` `emailFinderExecutor.ts`; `tr4m0ryp` `email_resolver/worker.py` | `agent_column.py` / `email_finder.py` agentic variant | The prompt + credit-guard rules turn finder hit-rate from ~40%→85%+. |
| ★ | **Declarative provider manifests** — add any provider as a ~30-line YAML (`auth`/`endpoint`/`bodyTemplate`/`response.mappings` JSONPath-lite/`pagination`/`smoke_test`) compiled to an executable `invoke()`; resolved via a **capability registry** with per-workspace priority overrides | `Othmane-Khadri/YALC` `src/lib/providers/{capabilities.ts,declarative/compiler.ts}` + `providers/manifests/**` | new `apps/api/services/leadgen/enrichment/declarative/` + capability registry | **Reframes the whole provider layer**: every other repo's provider becomes a YAML file instead of code. The single highest-leverage architectural port. |

---

## By subsystem

### A. Enrichment waterfall (`provider.py` / `planner.py`)

**Core engine upgrades (P0):**
- **`masteranime/enrichment-kit`** `src/waterfall/index.ts` — the cleanest distillation of "the Clay innovation": effective-cost sort (`costPerCall/typicalHitRate`), cache-first, best-of-N with `earlyExitOnConfidence=0.85`, validate-before-accept, cache-winner. Port the loop into `WaterfallEnricher.enrich`.
- **`nimajnebrevilo/GTM-Engine`** `src/db/queries/enrichment-cache.ts` — production cache semantics: per-`(provider, lookup_key, lookup_type)` rows + `expires_at` + `credits_used`, and crucially **`getAnyCachedEnrichment`** (cross-provider freshest hit). Plus `src/dedup/{matcher,normalizer}.ts` — 3-pass dedup (reg# → domain → fuzzy Levenshtein ≥0.90) + legal-suffix/diacritic/country-ISO normalizer → new `apps/api/services/leadgen/dedup/`.
- **`Revgrowth1/claude-code-skills`** `skills/enrichment/local-enrich/lib/` — drop-in Python toolkit: `email_waterfall.py` (BlitzAPI→Prospeo→IcyPeas, returns `(email, source)`, per-hop cost), `cost_tracker.py` (per-provider/per-step ledger + **cost_per_lead**), `lead_scoring.py` (A/B/C), `mx_classifier.py` (google/microsoft/other for ESP-aware sending), `checkpoint.py`+`throttle.py` (resumable + **live-adjustable concurrency** via control file), `tam-map/dedup_engine.py` (3-tier dedup w/ column-alias auto-map).

**Cost / billing model (P0):**
- **`sayanta-ghosh/gtm-engine`** `server/execution/{service,parallel,retry,rate_limiter,cache,normalizer}.py` — a near-1:1 Python version of Yupcha's planner+runner: `resolve→cache→rate_limit→retry→normalize→cache_store`, per-op `CACHE_TTLS` (enrich 7d/search 1h), `calculate_cost()` scaling, Redis token-bucket (atomic Lua), checkpointed concurrent batch executor.
- **`nurturev/gtm-engine`** `server/billing/*` + `server/vault/service.py` — BYOK-free/platform-charged billing with credit-hold→execute→confirm/release, and **per-tenant key vault** (dev: PBKDF2-HMAC from `JWT_SECRET` salted by `tenant_id`; prod: AWS KMS envelope w/ `EncryptionContext`). Directly fits Yupcha's per-workspace tenancy.
- **`LeadGrowGTM/discolike-cli`** `cost.py` — `CostTracker` with budget warnings at 80%/95%, raise at 100%, dry-run `estimate()`; `PLAN_PRICING` + `PLAN_GATED_FEATURES`.

**Concrete provider clients to lift (P0/P1)** — each comes with documented API quirks (months of trial-and-error):
- **Apollo**: `sayanta-ghosh` `apollo.py` (full op-map + param-mapping table + `clean_domain`/`ensure_list`), `nurturev` `apollo.py` (541 lines), `adeel0x01/apollo-mcp` (people-search + `bulk_match` ≤10 + Zod schemas), `codyschneiderx` `apollo.py` mapper. Apollo gotchas captured: rejects `https://`-prefixed domains, string-where-array params, `reveal_personal_emails` flag, rate-limit headers (`x-{minute,hourly,24-hour}-requests-left`).
- **RocketReach**: `nurturev` `rocketreach.py` (1026 lines) — async `status=progress` **polling** pattern, auth `Api-Key <key>`, POST search returns 201.
- **Prospeo**: `bcharleson/prospeo-cli` (complete: enrich/search/bulk person+company, free `search-suggestions` + `account-information`, `X-KEY` auth, credit costs — `enrich_mobile`=10 vs 1), `tr4m0ryp` `prospeo_finder.py` (**multi-key round-robin pool**, park 429 keys 1h, disable INVALID permanently).
- **LeadMagic**: `bcharleson/leadmagic-agent-cli` `src/core/{client,types}.ts` + commands — 21 endpoints with exact paths + per-call credit costs (email-finder 1cr, mobile 5cr, b2b-profile 10cr, technographics 1cr, job-change-detector 3cr, employee-finder 0.05cr/employee…) + RFC-9457 error parsing; `Andytoizer/agentoperator` `clients/leadmagic.py`+`findymail.py` (resumable disk cache, stop-on-valid/catch_all).
- **Verifiers**: `bricks` 8-vendor cascade (QEV/Verifalia/MillionVerifier/ZeroBounce/Emailable/Hunter/BillionVerify/Reoon — exact status-map tables), `tr4m0ryp` `smtp_verify/` (**raw port-25 RCPT + catch-all probe** with **HTTP-verifier fallback** for blocked-port cloud envs — the single most valuable verify port), `ALEXcERpto/Clay-alternative` `waterfallValidator.ts` (Prospeo→Icypeas w/ provenance + Bottleneck per-provider rate limits), `nimajnebrevilo` Million-Verifier client.
- **Neural/AI search providers**: `exa-labs/company-enrichment-demo` `api/enrich/route.ts` (Exa `type:'deep'`+`category:'company'`+inline `outputSchema` → **grounded JSON with citation URLs** + `maxAgeHours` cache — cleanest neural enricher), `nimajnebrevilo` `providers/exa/{search,triggers}.ts` (`findSimilar` lookalikes + `detectTriggers` funding/new_hire/leadership/expansion), `dishambles` Exa `searchAndContents`.
- **Others worth a provider stub**: `voxgig-sdk/company-enrich-sdk` (CompanyEnrich.com — lookalike/similar-company search), `bcharleson/ocean-agent-cli` (Ocean.io + headcount→bucket mapper), `nurturev` `predictleads.py` (signals/jobs/tech/funding, dual-key auth) + `parallel_web.py` + `fresh_linkedin.py` (RapidAPI), `thor-sen/lead-enrichment-pipeline` (PDL `/v5/company/enrich` mapper + "keep most-complete" dedup).

**Free / zero-cost first-stage sources (P1)** — run before paid providers:
- **`clod00/digitalclod-prospector`** `websiteCheck.js` (Meta-Pixel/GTM/WhatsApp detection + email/social/JSON-LD-address extract), `emailFinder.js` (homepage→`/contatti`→`info@`→**domain-guessing from company name**→Apify fallback).
- **`nilesh931/claygent-alternative`** `research_engine.py` (1412 lines) — zero-cost self-improving company research: `ddgs`→Playwright(crawlee)+cloudscraper→trafilatura→sentence-transformers semantic filter→score 8 dimensions→gap-targeted re-search. Includes anti-contamination logic (look-alike domain detection) + `PRIORITY_PATHS` (40+ company sub-pages).
- **`shivkcodes/Ai-Company-Enrichment-hackathon`** `company_enrichment_core.py` — battle-hardened email/phone/**JSON-LD PostalAddress** extractors with no-LLM fallbacks + model cascade (graceful degradation without a key).
- **`Vkdevnani`** / **`tafseerfatma`** / **`gandudileeladrireddy`** — `rapidfuzz`/`thefuzz` best-page selection (score links vs about/contact/services keywords, scrape top-N) + single-LLM extraction → `{core_service, pain_point, outreach_opener}`. Cuts LLM tokens by focusing on the right pages.
- **`elikem2021/gtm-engineering`** `abm/account_intel.py` `github_signals()` — free technographic via `api.github.com/orgs/{org}` (language + license mix). `m1227sasaki` `api/search.js` — 4-tier **company-name→official-domain** waterfall (domain-variations+title-score → Google-scrape → Claude judge → Claude web_search tool), tags which tier resolved.

### B. Workbook (columns / runner / source engine)

- **`eliasstravik/rowbound`** (the workbook gold) — `core/types.ts` action taxonomy (`http|waterfall|formula|exec|lookup|write|script|ai` + source types `http|exec|webhook|script`, each with `when`/`extract`(JSONPath)/`ifEmpty`/`onError`/`runSettings`); `core/engine.ts` per-row incremental loop (skip-if-filled, write-each-cell-immediately/crash-resilient, in-memory row update so downstream cols see new values, `dryRun`/`AbortSignal`); `core/action-deps.ts` topo-sort by `{{row.x}}` refs; `core/url-guard.ts` SSRF; `core/template.ts` (`{{row.x}}` + prototype-pollution guard); `core/http-client.ts` (429/5xx backoff + `onError` skip/stop/write).
- **`leszek-backpack/rex`** `clay_client.py` + `action-registry.md` — reverse-engineered Clay engine: action-column model (`actionKey`+`packageId`+`inputsBinding`, formula refs must be field-IDs), `run_and_wait` **cell-status state machine** (QUEUED/RUNNING/PENDING/DONE/ERROR), `_sort_by_deps` topo-sort, `set_condition` conditional runs, async CSV-export-to-S3 poll. The `action-registry.md` is a ready provider catalog.
- **`VAV-Technologies/clay-clone`** `campaign-executor.ts` — **dual-column enrichment output** (forensic "(AI)" result col showing which provider won + clean value col), `qualify_titles` (sample 8%→classify→delete "no" if unqualified-rate≥0.3), junk filters (`isUsableCompanyDomain`, `isValidEmailValue`, `resolveEnrichedText`).
- **`clawnify/open-table`** `components/table-row.tsx` — **dual human-vs-agent render mode** (`isAgent`: inline-edit for humans; stable `aria-label`/`data-row-id`/`data-col` form rows so the copilot can drive the grid deterministically) + `json_extract`-based dynamic-column sort/filter + agent-discoverable `/openapi.json`.
- **`exa-labs`** `LiveEnrichmentTable.tsx` + **`Itura-AI/lead-enrichment-agent`** (`Code.gs`+`callback_handler.py`) — per-row async status (`enriching→done/error`, `activeCount`) and a **cell-targeted async callback contract** (`{spreadsheetId, sheetName, rowNumber, statusColumnName}` + row token) — maps onto Yupcha's live-enrichment cells.
- **`MilosK88/sococo-plie-showcase`** `backend/api/reactivate.py` — parallel multi-source fan-out (`asyncio.gather(return_exceptions=True)` graceful degradation), **Redis job-progress counter** (`hincrby`, pollable), **per-tenant idempotency lock** (`SET NX EX`), bulk transactional `executemany`, 24h job-key TTL. Directly applicable to `workbook/worker.py`/`refresh.py`.

### C. Scraping / anti-bot / sources

- **`bricks`** `api/bricks-api/` — read/scrape waterfall (fetch+Readability → Puppeteer(tracker-blocked) → 6 paid scrapers) with **SPA-detection gate** (Readability-fails = JS page = cascade) + `isCaptchaOrChallenge()` (Cloudflare/Datadome/PerimeterX markers); **header hardening** (UA rotation + full `Sec-CH-UA`/`Sec-Fetch-*` + Google referer + 403/429-retry-with-fresh-UA); Puppeteer pool (page-reuse-with-state-clear, `BLOCKED_TRACKER_DOMAINS` ~60 incl. PostHog that hangs `networkidle2`, per-domain 1s rate limit). **Search failover** (Serper→Exa→Tavily→DDG-HTML-scrape, normalized `{title,url,snippet}`).
- **`Deepeshtolan8i/n8n-brightdata-challenge`** — Brightdata **SERP-as-JSON** (`google.com/search?...&brd_json=1` → structured `organic[]`) + async **snapshot pattern** (trigger→poll→download) for JS-heavy sites.
- **`ralcky/linkedin-job-intelligence`** — Playwright LinkedIn jobs scraper (`--disable-blink-features=AutomationControlled`, public `/jobs/search`, `f_TPR=r604800` recency) + **JD→tech-stack keyword extractor** + employee→revenue heuristic.
- **`What-Is-Love1/demo-outreach`** `scraper.js` + **`Revgrowth1`** `serper.py` — Serper.dev Google-SERP source (extracts `places` local-business phone/rating for free; `credits_exhausted` circuit-breaker on 400).
- **`Lead-Orchestra/awesome-b2b-leads`** — shopping list for the scraping engine (notably `curl_cffi`/curl-impersonate for anti-bot). **`onvoyage-ai`** — zero-dependency Node site crawler (sitemap+robots+internal-link, concurrency caps, schema.org detection).
- **`nimajnebrevilo`** `config/proxy.ts` — global undici `ProxyAgent` dispatcher (all `fetch` through a proxy, zero per-call changes).

### D. Copilot / AI columns / prompts

- **Prompt corpora (P0)**: `forma-norden/clay-claude-code-skill-pack` (6 AI-column templates w/ 5-part architecture + `INSUFFICIENT_DATA`; waterfall provider cost/quality table + role-email/catch-all filters; credit-optimization formula `rows×providers×(1−skip_rate)`; table-architect starter templates; clayscript formula library; **empirical benchmarks** — 22-30% annual email decay, 30-day re-verify cadence). `sachacoldiq/ColdIQ` (Noski's 8 prompting rules + `"purple"` null sentinel; qualification/opener/tech-stack prompts; signal taxonomy + 3-tier scoring). `mariosworkflows/GTM-Engine` 8-skill chain (**concentric tiers** T1⊂T2⊂T3, "revenue is not a valid signal — use headcount/funding/tech proxies"; 6-category signal detection w/ quality gate; **90-day new-hire** signal; verified-client-only 3-email rules).
- **AI service infra (P0)**: `psolerosell-stack/gtm-engine` `app/services/ai.py` — Claude wrapper with **per-call cost/token/latency logging + `prompt_hash` versioning** to an `ai_call_logs` table + `get_usage_stats()`; `_extract_json()` fence stripping. The observability layer Yupcha's enrichment lacks.
- **Scoring techniques**: `Prakharanand000/callbook-gtm-engine` `score_leads.py` — **TF-IDF cosine similarity vs a "golden ICP reference doc"** (no-LLM ICP-fit at scale) + K-Means buyer-archetype clustering. `codyschneiderx` `scorer.py` + `arshiagit` `icp-scoring.service.ts` + `psolerosell-stack` `scoring.py` — explainable weighted scoring with `get_score_breakdown()`/`reason` strings + exclusion/required/weighted rules + DB weight-versioning. `jing787/event-lead-cli` — **two-stage segmentation** (one LLM call defines thresholds → Python buckets all rows) + async-batched Instructor scoring with Python-computed weights ("don't trust LLM arithmetic").
- **Anti-hallucination / grounding**: `nuj201/gtm-engine` `_extract_verified_quotes` + `_quote_found_in_sources` (`SequenceMatcher` ≥0.75 substring check — auditable research); `vibhorkumar1209/gcc-intelligence-hub` 2-phase (fact-find → synthesize-grounded-only-on-facts); `Tekipeps` `synthesis.ts` (tiered source-reliability T1 own-domain→T4 aggregators + 0-1 confidence + `dataQuality.discrepancies` log); `ecdexclusive` derived-signals **with source-URL provenance**.
- **Agent loops**: `bricks` dual-agent writer/persona-critic (approve only at score ≥8/10) + `executor.ts` extracted-wait-time backoff + synthesis fallback; `dlipsy-supintel/jars_of_clay` Claude tool-use loop (`enrich_company`/`find_email`/`store`, 10-turn cap); `dishambles` `agents/harness.ts` (typed `CampaignPlan` names downstream agents → parallel dispatch w/ per-step status); `Itura-AI` search→extract-entity→re-search→merge.
- **Web-search tool wiring**: `VAV` `enrichment-tools.ts` (`search_web`/`scrape_url` in both Chat-Completions + Responses shapes + `WEB_SEARCH_SYSTEM_HINT`: search-once, smallest limit, don't loop + result compaction). `elikem2021` `llm.py` Perplexity backend for web-grounded research.

### E. MCP surface (if Yupcha ships one)

- **Auto-registration pattern (P1)**: define each op once as a Zod/`CommandDefinition` → register as both CLI command and MCP tool. Sources: `bcharleson/{leadmagic,ocean,prospeo}-cli` `src/mcp/server.ts`, `Br0ski777/x402-agent-tools` (catalog→`paramToZod`→`tool()` generator for AI-SDK + LangChain).
- **Tool schemas / Clay API shape**: `bpw-civic/clay-mcp-server` (`clay_search/enrich_company/person`, Mongo-style filter DSL `{$in,$gte,$contains}`, size-enum→range parser), `jbalbu01/sales-intelligence-mcp-server` (Clay/ZoomInfo/LinkedIn/Gong tools + MCP annotations `readOnlyHint`/`idempotentHint`), `adeel0x01/apollo-mcp` (full Apollo v1 surface).

### F. CRM / export integrations

- `codyschneiderx` `integrations/hubspot.py` (`_ensure_custom_properties()` auto-creates props before sync, idempotent upsert) + `elikem2021` `crm/{hubspot,salesforce,marketo,pardot}.py` (90-line httpx wrappers, search-by-email→patch-or-create true upsert) + `arshiagit` `crm-sync.service.ts`.
- `altafsnaptrude/attio-clay-enrichment` `src/attio_client.py` — **Attio** connector with robust attribute-type extractors (text/personal-name/record-reference) + status lifecycle (Yupcha has no Attio connector).
- `kayacancode/obsidian-gmail-crm` — Gmail OAuth (`gmail.metadata` scope only, privacy-preserving) + contact-frequency relationship graph.
- Export field-mapping tables (Apollo/Smartlead/Instantly/HubSpot camelCase quirks + merge-tag syntax + per-mailbox send limits): `forma-norden` `clay-outbound-export.md`; lemlist connector: `gtmadviser/cdtm-gtm-engine` `push_to_lemlist.py`.

---

## Build plan (sequenced)

**Phase 1 — Waterfall core (P0, highest ROI, all code-complete in the repos):**
1. `provider.py`: change stop condition to confidence-early-exit + best-of-N (port `masteranime` loop).
2. New `enrichment_cache` table + `canonical_key()` consulted before the chain & written on success (port `masteranime` `canonicalKey` + `nimajnebrevilo` `getAnyCachedEnrichment`).
3. `validateOutput()` accept-gate in the executor (port `masteranime` `validate.ts` + VAV junk filters).
4. DB-backed vendor/cost catalog + dynamic `calculate_cost()` (port `sayanta-ghosh`/`nurturev`).

**Phase 1.5 — Provider architecture (P0, foundational — do alongside Phase 1):**
- Port YALC's **capability registry + declarative YAML manifest compiler** (`enrichment/declarative/`). Once it exists, the new providers in later phases ship as ~30-line YAML manifests (Apollo/PDL/LeadMagic/Prospeo/Exa…) instead of bespoke clients — register them against capabilities with per-workspace priority overrides. ZoomInfo's user/pass→JWT auth needs a small code adapter (token-exchange auth type) rather than pure YAML.

**Phase 2 — Verify + finders (P0/P1):**
5. `email_verify_cascade.py` with the 4-status contract (port `bricks` status maps; add `tr4m0ryp` port-25+HTTP-fallback as free first layer); register existing + new vendors via the planner.
6. Agentic email-finder column (port `bricks` 4-phase prompt + `tr4m0ryp` waterfall + credit guards) wired to the verify cascade.
7. New providers: expand Apollo (people-search + bulk_match), add Prospeo/LeadMagic/Exa/Million-Verifier from the captured specs.

**Phase 3 — Free signal columns + dedup (P1):**
8. `tech_stack` + `hiring_signals` providers (port `rahulchhabria` detectors + ATS scrapers; `Tekipeps` Exa job-velocity).
9. `apps/api/services/leadgen/dedup/` (port `nimajnebrevilo` normalizer + 3-pass matcher).
10. Free first-stage website enricher (port `nilesh931` research engine or `shivkcodes` extractors).

**Phase 4 — Workbook engine + safety (P0/P1):**
11. Adopt `rowbound` action/source taxonomy + per-row incremental engine + dependency DAG + **`url_guard.py` (mandatory)**.
12. Dual-column enrichment output + `errors-only` re-run scoping + per-mode rate-limit caps (VAV + bricks).
13. AI-call observability table + prompt versioning (`psolerosell-stack`).

**Phase 5 — Prompts + scoring + copilot (P0/P1):**
14. Ship the `forma-norden`/`ColdIQ` prompt library as `ai_column.py` defaults + copilot prompt-authoring guidance.
15. Explainable scoring column + the TF-IDF-vs-golden-ICP no-LLM scorer + two-stage segmentation.
16. Grounding loop (`[VERIFIED]`-quote-vs-source) for research columns; concentric-tier + 90-day-new-hire signal logic.

---

## Security cautions (carried from sources — do NOT replicate)

- **`bricks`** disabled its untrusted-code security validation (`queue.ts`/`puppeteer/route.ts` dangerous-pattern checks commented out) and uses `new Function` on user formulas. **`rowbound`** uses `node:vm` and explicitly notes it is **not a security boundary**. Yupcha's Python formula/script columns must run in a **real sandbox** (RestrictedPython / subprocess / container), never `eval`/`exec`.
- Any user-supplied URL (HTTP columns, source engine) **must** pass the SSRF guard (#7) — including numeric/octal/hex IP encodings.
- **`gandudileeladrireddy/app.py`** contains a hardcoded leaked Gemini API key — do not copy it through.
- Several repos reverse-engineer Clay's internal API (`leszek-backpack/rex`, `VAV` `clay-api.ts`) via session cookies — useful as **design/spec reference only**, not for runtime dependence.
- Verify all pricing/model-id constants (repos reference dated `claude-sonnet-4`/`claude-opus-4.x` ids and 2025 prices) against current values before porting cost math.

---

## Coverage notes

- **150 repos analyzed** by 11 code-reading agents. ~30 were empty stubs / static landing pages / unrelated (e.g. `TSP66/Monte-Carlos-Loamer` is gold-prospecting geochemistry) — enumerated in the per-batch reports; omitted here.
- Highest-value repos overall: `Othmane-Khadri/YALC`, `bricks`, `sayanta-ghosh/gtm-engine`, `nurturev/gtm-engine`, `masteranime/enrichment-kit`, `nimajnebrevilo/GTM-Engine`, `eliasstravik/rowbound`, `leszek-backpack/rex`, `tr4m0ryp/clay-enrichment`, `VAV-Technologies/clay-clone`, `Revgrowth1/claude-code-skills`, `forma-norden/clay-claude-code-skill-pack`, `rahulchhabria/local-enrichment-tool`, `mattvinall/Quick-Enrich-Tools`, `LeadMagic/leadmagic-openapi`, `HiAbhishekh/company-intelligence-enricher`.

---

## G. Provider architecture — YALC (`Othmane-Khadri/YALC-the-GTM-operating-system`, 225★)

The standout of the whole corpus: a mature, test-heavy ("open-source Clay, AI-native GTM OS") whose **declarative-provider + capability-registry** layer is essentially Clay's waterfall engine, generalized so non-engineers add providers by config.

- **Capability registry + priority resolution (P0)** — `src/lib/providers/capabilities.ts`. Skills declare WHAT they need (`capability: people-enrich`); a registry resolves a concrete provider via a per-capability priority list (user `~/.gtm-os/config.yaml` overrides the default), first `isAvailable()` wins, else throws `CapabilityUnsatisfied` with the ordered list it tried. → Port `Capability`/`CapabilityAdapter`/`CapabilityRegistry` to Python; per-workspace priority overrides slot into Yupcha's tenancy. This is the waterfall, generalized to capabilities (people-enrich/email-find/company-enrich/phone-find/icp-company-search).
- **Declarative provider manifests (P0)** — `src/lib/providers/declarative/{compiler,types}.ts` + `schema.json` + `providers/manifests/<capability>/<provider>.yaml`. A YAML (`auth`, `endpoint`, `request.bodyTemplate`, `response.mappings`, `pagination`, `smoke_test`) compiles to `invoke(input)`: resolves `${env:VAR}` (throws `MissingApiKeyError`), renders `{{input.x | default: 5}}` Mustache-lite into URL/headers/body, fetches, detects vendor error envelopes, projects response onto the capability output via JSONPath-lite mappings (`"companies[].domain": "$.primary_domain"`, array fan-out, equality, prefix-literal), cursor/page pagination with a hard limit. Lift-ready manifests: `icp-company-search/apollo.yaml`, `people-enrich/peopledatalabs.yaml`, `crm-contact-upsert/hubspot.yaml`, `email-campaign-create/brevo.yaml`. → Build `enrichment/declarative/` (Pydantic manifest + compiler returning a callable, registered into the capability registry). **Makes every other provider in this corpus a YAML file.**
- **Credit-cost model (P1)** — `src/lib/services/crustdata.ts` `CREDIT_COSTS` + `estimateCost(op, params) → {credits, breakdown}` (DB-search-first, free autocomplete, cached 1cr vs realtime 4cr). → per-provider `estimate_cost()` surfaced in the workbook run-preview before spend.
- **Human-gate engine (P1)** — `src/lib/frameworks/gates.ts` + `orchestrator/types.ts` `Gate{plan|data|action}`: approve/reject/resume state machine (idempotent approve, approve-after-reject=409). → workbook spend-approval gates ("enrich N rows = X credits, proceed?").
- **Pipeline/chain executor (P1)** — `src/lib/orchestrator/chain.ts` + `configs/pipelines/*.yaml`: sequential steps with `from`(prior output)/`condition`/`transform`, checkpoint/resume, retryable classification → informs `planner.py` as a declarative multi-step column pipeline.
- **Qualification + drift (P1)** — `src/lib/qualification/{pipeline,icp-gate}.ts`: `requireClientICP()` hard gate, `looselyMatch()` (50% token overlap), `computeDriftFlags()` (title_mismatch / ex-employer-in-headline / role-change <30d — catches stale titles, a differentiator).
- **AI workflow planner (P1)** — `src/lib/ai/workflow-planner.ts`: Anthropic `Tool` defs (`find_leads`/`enrich_leads`/`qualify_leads`) + system prompt injecting ICP + connected providers, compiles a tool call → step list with provider auto-selection + `requiredApiKeys`. Maps onto Yupcha's copilot→`planner.py`.
- **Plus (P1):** `crypto.ts` AES-256-GCM key encryption, `db/schema.ts` (workflow/result_sets/result_rows = clean enriched-row schema), `cached-fetch.ts` (partial-result-survives-crash), `predictleads.ts` (dual-header signals provider), markdown-frontmatter skill authoring.

**Batch-4 companion finds:** `LeadMagic/leadmagic-openapi` — full OpenAPI 3.1 (19 endpoints, `X-API-Key`, exact request/response + per-call credit costs: email-validate 0.05cr, email-finder 1cr, b2b-profile 10cr, company-funding 4cr, searchads 8cr…) → a complete Yupcha source + manifests + the credit table feeds the spend gate. `mattvinall/Quick-Enrich-Tools` (async-FastAPI, closest stack match) — **3-pass scrape escalation** (`scraper.py`: datacenter 1cr → JS-render 5cr → super+render 25cr, gated on content-length <1500, anti-bot-domain skip-list, SSRF guard, plan-concurrency semaphore) [P0 for the scraping source] + relevance-scored crawl + Retry-After-honoring retry. `HiAbhishekh/company-intelligence-enricher` — free BuiltWith-lite tech-detector (7 categories, ~100 patterns) + full URL-enrichment with hiring-signal probe + 0-100 lead score. `EvZaSi/zoominfo-on-demand-callout` — ZoomInfo auth (user/pass→JWT) + enrich flow + industry/NAICS standardization (the one provider needing a code adapter, not pure YAML, for its token-exchange auth). `rangapin/gtm-engine` — two-stage budget gate + DQ-pre-filter + Apollo→Exa fallback-on-credit-error heuristics. `ataata107/gtm_engine` — `circuit_breaker.py` (CLOSED/OPEN/HALF_OPEN) to fail-fast a down provider and fall through the waterfall.
