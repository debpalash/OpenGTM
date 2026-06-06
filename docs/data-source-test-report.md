# Data Source Test Report

> **UPDATE 2026-06-06 (after search fix):** re-ran `scripts/test_sources.py --all`
> through the new resilient search client (`get_ddgs` → `_ResilientDDGS`:
> direct/proxy fallback + multi-engine backends). **Result: 91 / 91 registry
> sources return data — 100%, 0 empty, 0 fail** (was 26/89 ≈ 29%). The original
> empties were caused by the proxy pool serving dead SOCKS proxies (every proxied
> search → ConnectError → empty), not by the source list or single-engine DDG.
> The fix is in `services/leadgen/proxy_client.py`; everything below is the
> ORIGINAL pre-fix run, kept for the record.
>
> **MULTI-PHRASE VALIDATION (4 phrases × geos: software/Bangalore, marketing/NY,
> manufacturing/Mumbai, consulting/London):** all **91/91 sources work** (return
> data on ≥1 phrase). 80 are solid on all 4 phrases; 8 at 3/4; 1 at 2/4
> (reddit_companies); 2 query-sensitive at 1/4 (forbes, quora_companies). Each
> individual phrase covered 85–88 of 91 sources. Confirms the fix holds across
> query types, not just one.

---

**Date:** 2026-06-06
**Scope:** All 96 sources in the Sources UI (`routers/settings.py:DATA_SOURCES`).
**Method:** For each registry source, ran the exact production path —
`build_queries(source, "software companies", "Bangalore")[0]` → `_ddg_search(q, max_results=8)`
(via `scripts/test_sources.py`), 1 retry on empty/error. Tested in parallel across 6 workers.

## Headline

| Bucket | Count |
|---|---|
| Registry sources **returning data** (≥1 DDG result) | **26 / 89** (29%) |
| Registry sources **returning nothing** (empty after retry) | 63 / 89 |
| Hard failures / timeouts | 0 |
| Registry-strategy source with **no registry definition** | 1 (`yellowpages_in`) |
| Special-strategy scrapers (not DDG; tested separately below) | 6 |

> **~29% of registry sources return any data via the DDG `site:` strategy.** The
> rest come back empty — DuckDuckGo either doesn't index that domain for `site:`
> queries, the site blocks crawlers, or it rate-limited us. Notably, most **major
> platforms** (Crunchbase, LinkedIn, Clutch, Capterra, Product Hunt, Trustpilot,
> AngelList/Wellfound, Indeed, Naukri, Yelp) returned **nothing**.

### Important caveats
1. **"OK" = DuckDuckGo returned ≥1 result, NOT that the results are good leads.**
   Several OK sources returned noise (e.g. `monsterindia` → a `.swf` file,
   `tradeindia` → "Network Adapter Card", `indiamart` → a "Start Now It's Free" ad).
2. **Empty ≠ permanently dead.** A 0 can be transient DDG rate-limiting/dead proxy.
   The harness retries once, which softens but doesn't eliminate this. Re-running
   may flip a few EMPTY→OK and vice-versa.
3. **Production only runs the top ~20 by priority.** `job_runner._search_registry_sources`
   sorts by `priority` and takes the **top 20**, and skips
   `{clutch, goodfirms, ambitionbox, linkedin_companies}` (handled by dedicated
   strategies). So a live job never queries all 89 — this test measures *capability*,
   not what one job does.

---

## ✅ Sources that returned data (26)

| Source | Results | Sample / note |
|---|---|---|
| indiamart | 8 | ad result ("All-in-One Business Software") — noisy |
| justdial | 8 | justdial.com |
| tradeindia | 8 | "Network Adapter Card" — off-target |
| sulekha | 8 | sulekha.com/bpo-companies/bangalore |
| exportersindia | 8 | "IT Software at Best Price in Bangalore" |
| glassdoor (global) | 5 | top-companies-bangalore listing |
| glassdoor_in | 8 | ad result — noisy |
| monsterindia | 8 | `.swf` asset — junk |
| facebook_pages | 8 | "Giridhara Software Services Pvt Ltd \| Bangalore" — good |
| google_reviews | 8 | AmbitionBox review page |
| g2 | 8 | "Global Software Companies for 2026 \| G2" — good |
| saashub | 8 | "Best Mobile App Development Company" |
| mouthshut | 8 | "List of EMPLOYER in India" |
| github_orgs | 8 | "Bangalore-startups-companies-list" — good |
| dial4trade | 8 | "Payroll Software in Bangalore" |
| manta | 8 | Manta.com company profile |
| generic_incubator | 8 | "Top 23 Accelerators and Incubators in Bangalore" — good |
| upwork | 6 | "Webure Technologies \| Upwork Company Profile" — good |
| livemint | 8 | Bengaluru startup news |
| dnb | 8 | D&B company data |
| techbehemoths | 8 | "Top 10+ ... Companies in Bangalore" — good |
| fiverr | 8 | agency listing |
| opencorporates | 8 | company listing |
| freelancer | 8 | "Bangalore software companies Jobs" |
| grotal | 8 | "Haulage Software in Chennai" — off-geo |
| dealroom | 8 | "Companies \| Dealroom.co" — good |

**Strong, on-target:** facebook_pages, g2, github_orgs, generic_incubator, upwork,
techbehemoths, dealroom, opencorporates, dnb.

---

## ⬜ Sources that returned nothing (63)

Grouped roughly by why they likely fail:

**Major platforms that block/aren't `site:`-indexed on DDG (high-value, worth fixing):**
crunchbase, linkedin_companies, clutch, goodfirms, capterra, getapp, stackshare,
producthunt, trustpilot, angellist (wellfound), toptal, sourceforge, tracxn,
cbinsights, nasscom, companieshouse, indeed, naukri, shine, yelp, foursquare,
tripadvisor, glassdoor (the .co.in/global pair was inconsistent).

**India directories/gov returning empty:**
zaubacorp, tofler, fundoodata, hotfrog, brownbook, cylex, kompass, europages,
startup_india, economic_times, business_standard, livemint(?), urbanpro, bark,
thomasnet, dnb(global OK / others no), yellowpages, grotal(OK).

**Startup/news/social returning empty:**
inc42, yourstory, techcrunch, hackernews, devto, forbes, reddit_companies,
quora_companies, twitter_companies, instagram_business, google_news, alternativeto,
appsumo, amazon_sellers, flipkart_sellers, shopify_stores.

**"Generic" heuristic queries (no `site:` scope) returning empty:**
generic_association, generic_expo, generic_awards, generic_govt_tender,
generic_iso_certified, generic_hiring_surge, generic_companies_list.

> Full per-source raw data (status, query, sample) is reproducible with
> `python scripts/test_sources.py --all`.

---

## ⚙️ Special-strategy sources (6) — not DDG `site:` registry

These use dedicated scrapers, tested by mechanism (not the harness above):

| UI source | Strategy | Mechanism | Status |
|---|---|---|---|
| duckduckgo | `web` | DDG text search (core path) | **Working** — it's the engine behind every OK above |
| google_maps | `maps` | Patchright headless browser | **Unavailable** — `patchright` not installed in the venv |
| directories | `directories` | web-directories scraper (DDG-backed) | Partial — same DDG fragility |
| linkedin | `linkedin` | DDG `site:linkedin.com/company` | **Weak** — the registry equivalent (`linkedin_companies`) returned EMPTY |
| job_boards | `job_boards` | Naukri/Indeed scrapers | **Weak** — `naukri` & `indeed` both EMPTY via DDG |
| ambitionbox | `review_sites` | dedicated AmbitionBox scraper | Untested here — needs live-pipeline run |

---

## 🔧 Config gap

- **`yellowpages_in`** is listed in `DATA_SOURCES` (strategy `registry`) but has **no
  entry in `source_registry.SOURCES`** → it can never run. Either add a registry def
  or remove it from the UI list.
- Inverse: `mouthshut_biz` exists in the registry but isn't exposed in `DATA_SOURCES`
  (orphan def). (`mouthshut` — a different id — is exposed and tested OK.)

---

## Recommendations

1. **The DDG `site:` strategy is the bottleneck, not the source list.** 71% empty is
   mostly DuckDuckGo not surfacing `site:domain` results for major platforms +
   rate-limiting. Options: (a) add a second search backend (Google CSE / Bing /
   SerpAPI) as a fallback when DDG returns 0; (b) for high-value blockers
   (Crunchbase, LinkedIn, Clutch, Capterra, Product Hunt), use their APIs or
   dedicated scrapers instead of `site:` search.
2. **Filter the noise.** Several OK sources return ads/assets/off-geo junk. The
   downstream `validate_lead_light` catches some, but the `site:` templates for
   indiamart/monsterindia/tradeindia/grotal should be tightened.
3. **Fix the config gaps** (`yellowpages_in`, `mouthshut_biz`).
4. **Install the browser tier** (`patchright`, `curl_cffi`) to enable `google_maps`
   and stronger anti-bot scraping — currently unavailable (also the cause of the
   stalled-job browser-tier fallbacks).
5. Re-run `scripts/test_sources.py --all` periodically — empties are partly
   environmental (proxy/rate-limit), so this is a moving baseline, not a fixed verdict.
