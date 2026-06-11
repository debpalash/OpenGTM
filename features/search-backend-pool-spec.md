# Search Backend & SearXNG Pool — Enrichment Data Substrate
## Technical Spec & Design

> Companion to `workbook-v2-source-engine-spec.md`. That spec assumes a working
> web-search substrate underneath the enrichment waterfall. This spec defines
> that substrate: how Yupcha acquires web data for **website-independent**
> enrichment, so cells stop returning `no_data` when a company's own site is
> dead and DuckDuckGo is blocked.

---

## 0. The One-Sentence Bet

> Replace direct, single-IP DuckDuckGo calls with a **pool of self-hosted SearXNG
> instances fronted by a concurrent Go gateway**, and layer **registry/API data
> sources** on top — so every website-independent provider has a reliable, free,
> high-concurrency backend.

---

## 1. Where We Are Today (grounded in the code)

The enrichment waterfalls (`services/workbook/enrichment.py::DEFAULT_WATERFALLS`)
fall into two classes:

- **Website-dependent** (`deep_scraper`, `jsonld_firmographics`, `website_scraper`,
  `tech_stack`) — need the target's own site reachable. Dead site → nothing.
- **Website-independent** (`ddg_company`, `social_finder`, `crosslinked`,
  `local_business`, `email_harvester`, `company_intel`, `decision_maker`) — all
  funnel through **DuckDuckGo** via the `ddgs` library.

**The break:** for small/obscure firms (e.g. Indian IT-staffing companies) the
site is often dead AND DuckDuckGo now CAPTCHA/rate-limits our single server IP.
Both classes collapse → `no_data`. Measured on workbook `4a994207…`: data-field
fill rate ~53–73%, every miss `no_data`. (AI columns are a separate LLM issue,
fixed elsewhere.)

## 1.5. Hard Findings — Validated Live (constrain the design)

Ran a real SearXNG container + probed public instances. Non-negotiable facts:

1. **From our server IP, the big HTML-scraping engines are blocked.** Google,
   DuckDuckGo, Brave, Startpage → CAPTCHA/"too many requests". **Bing, Qwant,
   Yahoo work.** → Default to `engines=bing,qwant,yahoo`; recover Google-grade
   results only via a **keyed engine** (Brave API / Serper) or **egress proxies**.
2. **Public SearXNG instances cannot be a JSON backend.** Direct probe of 8
   A/B-grade instances → **all HTTP 429/403 on the first JSON request**; pool
   health-check → **1/25 JSON-capable** (only ours). → The pool must be **our
   own** instances; public ones are a near-always-skipped bonus tier.
3. **SearXNG's own `limiter` must be off** for programmatic JSON, or it bot-gates
   our calls (`X-Forwarded-For nor X-Real-IP header is set!`).

## 1.6. Reuse Map — Build-On vs Build-New

| Concern | Reuse | Build |
|---|---|---|
| Metasearch engine | `searxng/searxng` (official Docker) | thin `settings.yml` (`infra/searxng/`) |
| Concurrency / pooling / failover | — | `apps/searxng-pool/` (Go, stdlib) ✅ built |
| Provider waterfall, circuit breaker, retry | existing `enrichment.py` / `planner.py` | repoint search providers to the pool |
| Community SearXNG patches | upstream + forks | `infra/searxng/sync-fork.sh` "union fork" ✅ built |

---

## 2. Architecture

```
enrichment providers (Python)          [search-based: ddg_company, social_finder,
  │  searxng_search(q, engines)         crosslinked, local_business, email_harvester,
  ▼                                     company_intel, decision_maker]
apps/searxng-pool   (Go :8889)   ── health-checks (liveness + JSON + latency)
  │  hedged fan-out, merge+dedup        ── politeness gap, latency/fail-aware pick
  ▼
pool of SearXNG instances (JSON API)
  ├─ infra/searxng  (ours, :8888)  ← PRIMARY (reliable, we control it)
  ├─ ours #2..N     (+ egress proxy each)  ← scale / IP diversity
  └─ public feed (searx.space)     ← bonus tier, usually JSON-blocked
        engines: bing, qwant, yahoo [+ keyed Brave/Serper]
```

Two layers, never conflated:
- **Acquire web data** (this spec): SearXNG pool ± keyed search API.
- **Search our own data** (out of scope; later): Sonic/Meilisearch/`hister` over
  cached pages — *not* a web-data source.

---

## 3. Components

### 3.1 SearXNG instance — `infra/searxng/settings.yml`
`use_default_settings: true`; `server.limiter: false`; `search.formats: [html, json]`.
Run via Colima/Docker on `:8888`. One container today; N later (§6).

### 3.2 Go pool gateway — `apps/searxng-pool/` (built)
Stdlib-only; single static binary.

**Data model** (`pool.go`):
```
Instance{ URL, Grade, healthy, jsonOK, latencyMs, fails, lastUsed, client(proxy?) }
Pool{ instances[], cfg }   // RWMutex-guarded
```
**Health loop:** every `HEALTH_INTERVAL`, probe each instance concurrently
(bounded 12) with a cheap JSON query; set `healthy`/`jsonOK`/`latencyMs`. A
non-JSON or non-200 response demotes the instance.

**Selection (`Pick`):** healthy ∧ jsonOK ∧ `now-lastUsed ≥ PerInstanceGap`,
scored by `latency + fails·500 + inFlight·2000` (lower first).

**Search:** pick `Fanout` instances, query concurrently with per-try timeout,
**merge + URL-dedup** (canonicalized host/path), return once `MinResponses`
answered or the budget expires; failures bump `fails` and demote at ≥3.

### 3.3 Python helper — `services/workbook/searxng_search.py` (TODO)
```python
async def searxng_search(query: str, *, engines: str | None = None,
                         max_results: int = 10) -> list[SearchHit]:
    # GET {SEARXPOOL_URL}/search?q=&engines=&n=  → [{title,url,content,engines}]
    # used by the search-based providers in place of `ddgs`
```
Resilient: on pool error, fall back to direct SearXNG (`SEARXNG_URL`), then to
the legacy `ddgs` path (so this is a strict upgrade, never a regression).

### 3.4 Provider repoint (TODO)
Replace the `ddgs`/DuckDuckGo calls inside `ddg_company`, `social_finder`,
`crosslinked`, `local_business`, `email_harvester`, `company_intel`,
`decision_maker` with `searxng_search(...)`. No waterfall/order changes — these
already sit in `DEFAULT_WATERFALLS`; they just get a working backend.

### 3.5 New website-independent data sources (TODO, prioritized)
From the deep-research report (`reference/research/enrichment-data-sources-deep-research.json`).
Work from company **name/domain**, no site needed:

| Provider (new) | Fills | Access | India |
|---|---|---|---|
| `mca_master_data` | address, status, CIN, RoC, incorporation | data.gov.in free API + bulk ZIP | ★ core |
| `instafinancials` | **directors (decision-makers)**, reg. email/website, firmographics | free JSON/XML, ~1.8M cos | ★ core |
| `brave_search` / `serper` engine *inside SearXNG* | revives Google-grade results | keyed (free tier) | global |
| `gleif_lei` | legal name, address (LEI) | free public API | thin |
| `wikidata` | firmographics for known entities | free SPARQL, keyless | thin |
| `leadmagic` / `findymail` | email + mobile from name+domain | freemium (100 free) | global |

---

## 4. API Surface

**Pool gateway (`:8889`)**
```
GET /search?q=<query>&engines=<csv>&n=<int>
    → {query, results:[{title,url,content,engines[]}], instances_used[], elapsed_ms}
GET /health  → {ok, instances, healthy, json_capable}
GET /pool    → [{url,grade,healthy,json_ok,latency_ms,fails}]
```
**No new Yupcha REST routes** — this sits below the providers. Settings exposed
via the existing enrichment-perf settings if we surface engine/pool config.

---

## 5. Config Reference (env)

| Var | Default | Notes |
|---|---|---|
| `SEARXPOOL_ADDR` | `:8889` | gateway listen addr |
| `SEARXPOOL_SEED` | `http://localhost:8888` | our instances (CSV) |
| `SEARXPOOL_PROXIES` | — | egress proxies, round-robined (CSV) |
| `SEARXPOOL_PUBLIC_FEED` | `true` | include searx.space (mostly skipped) |
| `SEARXPOOL_DEFAULT_ENGINES` | `bing,qwant,yahoo` | working engines from our IP |
| `SEARXPOOL_FANOUT` / `_MIN_RESPONSES` | 4 / 2 | concurrency vs latency |
| `SEARXNG_URL` (Python) | `http://localhost:8888` | direct fallback |
| `SEARXPOOL_URL` (Python) | `http://localhost:8889` | primary |

---

## 6. Phased Delivery

- **P0 — backend live (DONE):** self-hosted SearXNG + Go pool built, verified
  `pool → searxng → bing/qwant → deduped results`. Union-fork tooling.
- **P1 — repoint providers:** `searxng_search()` helper + swap the 7 search
  providers off `ddgs`. *This is what closes `no_data` on existing fields.*
- **P2 — registry sources:** add `mca_master_data` + `instafinancials` providers
  and wire into `address/phone/decision_makers/description` waterfalls.
- **P3 — scale & quality:** multi-instance `docker-compose` + per-instance
  egress proxies; add a keyed Brave/Serper engine; spam-filter qwant results.

---

## 7. Acceptance Criteria

- [ ] P1: the 7 providers query the pool; with SearXNG up, a dead-site company
      returns ≥1 real result for `linkedin_url`/`phone`/`description` where raw
      DDG returned `no_data`. Strict fallback to `ddgs` if the pool is down.
- [ ] P1: `searxng_search` adds < 1.5s p50 latency per call.
- [ ] P2: `instafinancials` fills `decision_makers` for an Indian firm whose
      site is dead (directors from the registry).
- [ ] P3: pool serves with ≥2 of our instances; one instance down ⇒ no failures.

---

## 8. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Bing/Qwant also start blocking our IP | egress proxies per instance; keyed Brave/Serper engine |
| Qwant returns spam results | per-engine result filtering; prefer bing; drop low-quality TLDs |
| Pool down → enrichment regresses | strict fallback chain: pool → direct SearXNG → `ddgs` |
| Public instances tempting but JSON-blocked | treated as bonus tier, never depended on (validated) |
| Registry APIs change / rate-limit | cache by company key; keep waterfall fallbacks |
| SearXNG self-host ops burden | single Docker container; union-fork keeps it current |

---

## 9. Why This Wins

DuckDuckGo on one IP was a single point of failure throttling every
website-independent provider at once. A self-hosted SearXNG **pool** turns search
into owned, horizontally-scalable infrastructure — free, no per-call vendor
cost — and the registry layer (MCA/InstaFinancials) adds data that exists
*independent of the target's website*. Together they convert the dominant
`no_data` failure into real coverage, at $0 marginal cost, for exactly the
obscure-SMB segment Clay-style vendors charge the most to reach.

---

## Status
- ✅ P0: `infra/searxng/`, `apps/searxng-pool/` (Go), `sync-fork.sh`.
- ✅ P1: `web_search.py` shim → repointed 10 providers (search off DDG).
- ✅ P2: `wikidata` + `mca_registry` providers, wired into the waterfalls.
- ✅ P3: qwant **spam filter** in the pool (drops single-engine http junk),
  **multi-instance `docker-compose.yml`** + pool `Dockerfile` (distroless,
  proxy-ready), and a documented **keyed Serper engine** + proxy scale path.
- ⬜ Activation (config, not code): `DATA_GOV_IN_KEY`, Apollo/Hunter keys,
  egress proxies / Serper key for premium engines.
- Refs: `reference/research/searxng-search-backend-eval.md`,
  `reference/research/enrichment-data-sources-deep-research.json`.
