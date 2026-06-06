# Sources — Master Inventory & Enhancement Plan

Authored 2026-06-06. Consolidates every "source" scattered across the project
into one document, then lays out an enhancement plan per source / category.

## Where sources live (4 layers)

| Layer | Where | Count | Role |
|---|---|---|---|
| **UI source list** | `routers/settings.py` `DATA_SOURCES` | 96 | what the Sources page shows + enable toggles |
| **Registry (data-driven)** | `services/leadgen/source_registry.py` `SOURCES` | 91 | DDG `site:` discovery sources (config, not code) |
| **Special scrapers** | `services/leadgen/scrapers/*.py` | 9 | the 6 UI "strategies" + crunchbase/web/csv |
| **Enrichment providers** | `services/leadgen/enrichment/providers/*.py` | 26 | enrich existing leads (not discovery) |
| **Misc** | ambitionbox API, person_intel, legacy stubs | 5 | native API, person research, dead stubs |

96 UI sources = 90 registry-strategy (89 with a registry def) + 6 special scrapers.
Registry has 91 defs (1 — `mouthshut_biz` — not surfaced in the UI).

---

# PART 1 — THE INVENTORY (all sources collected)

## A. Lead-discovery: Registry sources (91)

DDG `site:`-scoped searches. Templates use `{q}`/`{industry}`/`{city}`. "Qual"
column: ✅ solid · ⚠️ noisy/listing-page · ❌ weak template (low recall).

| # | name | category | region | #tmpl | extract_listing | Qual |
|---|---|---|---|---|---|---|
| 1 | indiamart | b2b_marketplace | india | 3 | – | ⚠️ ads/.swf noise |
| 2 | justdial | directory | india | 2 | – | ✅ |
| 3 | tradeindia | b2b_marketplace | india | 1 | – | ⚠️ off-target |
| 4 | sulekha | directory | india | 1 | – | ✅ |
| 5 | exportersindia | b2b_marketplace | india | 1 | – | ✅ |
| 6 | ambitionbox | review | india | 1 | – | ✅ (also native API) |
| 7 | glassdoor_in | review | india | 1 | – | ✅ |
| 8 | naukri | jobs | india | 1 | – | ✅ |
| 9 | zaubacorp | government | india | 1 | – | ✅ (registration data) |
| 10 | tofler | government | india | 1 | – | ✅ (registration data) |
| 11 | clutch | directory | global+ | 2 | ✔ | ⚠️ needs listing scrape |
| 12 | goodfirms | directory | global+ | 2 | ✔ | ⚠️ needs listing scrape |
| 13 | g2 | review | global | 1 | ✔ | ⚠️ needs listing scrape |
| 14 | softwaresuggest | directory | global+ | 1 | ✔ | ⚠️ needs listing scrape |
| 15 | techbehemoths | directory | global+ | 1 | – | ✅ |
| 16 | yellowpages | directory | us | 1 | – | ✅ |
| 17 | yelp | directory | us | 1 | – | ✅ |
| 18 | bbb | directory | us | 1 | – | ✅ |
| 19 | thomasnet | b2b_marketplace | us | 1 | – | ✅ |
| 20 | crunchbase | startup | global | 2 | – | ✅ |
| 21 | tracxn | startup | global+ | 1 | – | ✅ |
| 22 | angellist (wellfound) | startup | global | 1 | – | ✅ |
| 23 | yourstory | startup | india | 1 | – | ✅ |
| 24 | inc42 | startup | india | 1 | – | ✅ |
| 25 | linkedin_companies | social | global | 1 | – | ✅ |
| 26 | facebook_pages | social | global+ | 1 | – | ✅ |
| 27 | indeed | jobs | global+ | 1 | – | ✅ |
| 28 | glassdoor | review | global/us | 1 | – | ✅ |
| 29 | europages | b2b_marketplace | eu | 1 | – | ✅ |
| 30 | kompass | b2b_marketplace | eu+ | 1 | – | ✅ |
| 31 | capterra | saas_directory | global | 1 | ✔ | ⚠️ needs listing scrape |
| 32 | getapp | saas_directory | global | 1 | ✔ | ⚠️ needs listing scrape |
| 33 | producthunt | saas_directory | global | 1 | – | ✅ |
| 34 | sourceforge | saas_directory | global | 1 | – | ✅ |
| 35 | alternativeto | saas_directory | global | 1 | – | ✅ |
| 36 | saashub | saas_directory | global | 1 | – | ✅ |
| 37 | stackshare | saas_directory | global | 1 | – | ✅ |
| 38 | appsumo | saas_directory | global | 1 | – | ✅ |
| 39 | trustpilot | review | global+ | 1 | – | ✅ |
| 40 | tripadvisor | review | global | 1 | – | ⏸ disabled |
| 41 | google_reviews | review | global | 1 | – | ✅ (no site: scope) |
| 42 | mouthshut | review | india | 1 | – | ✅ |
| 43 | upwork | freelance | global | 1 | – | ✅ |
| 44 | fiverr | freelance | global | 1 | – | ⏸ disabled |
| 45 | toptal | freelance | global | 1 | – | ⏸ disabled |
| 46 | freelancer | freelance | global | 1 | – | ⏸ disabled |
| 47 | bark | freelance | global/us | 1 | – | ✅ |
| 48 | github_orgs | developer | global | 1 | – | ✅ |
| 49 | stackoverflow_jobs | developer | global | 1 | – | ✅ |
| 50 | hackernews | developer | global | 1 | – | ✅ |
| 51 | devto | developer | global | 1 | – | ⏸ disabled |
| 52 | google_news | news | global | 1 | – | ✅ |
| 53 | economic_times | news | india | 1 | – | ✅ |
| 54 | business_standard | news | india | 1 | – | ✅ |
| 55 | livemint | news | india | 1 | – | ✅ |
| 56 | techcrunch | news | global | 1 | – | ✅ |
| 57 | forbes | news | global | 1 | – | ❌ strict-phrase, 1/4 |
| 58 | mouthshut_biz | directory | india | 1 | – | ⚠️ **not in UI**, mislabelled |
| 59 | grotal | directory | india | 1 | – | ⚠️ ad noise |
| 60 | urbanpro | directory | india | 1 | – | ✅ |
| 61 | dial4trade | b2b_marketplace | india | 1 | – | ✅ |
| 62 | fundoodata | directory | india | 1 | – | ✅ |
| 63 | startup_india | startup | india | 1 | – | ✅ |
| 64 | nasscom | directory | india | 1 | – | ✅ |
| 65 | shine | jobs | india | 1 | – | ✅ |
| 66 | monsterindia | jobs | india | 1 | – | ⚠️ job-board-as-dir |
| 67 | manta | directory | us | 1 | – | ✅ |
| 68 | dnb | directory | global/us | 1 | – | ⏸ disabled |
| 69 | opencorporates | government | global/eu | 1 | – | ✅ |
| 70 | companieshouse | government | eu | 1 | – | ⏸ disabled |
| 71 | dealroom | startup | global/eu | 1 | – | ✅ |
| 72 | cbinsights | startup | global | 1 | – | ✅ |
| 73 | foursquare | directory | global/us | 1 | – | ⏸ disabled |
| 74 | hotfrog | directory | global | 1 | – | ✅ |
| 75 | brownbook | directory | global | 1 | – | ✅ |
| 76 | cylex | directory | global/eu | 1 | – | ✅ |
| 77 | twitter_companies | social | global | 1 | – | ✅ |
| 78 | instagram_business | social | global+ | 1 | – | ✅ |
| 79 | reddit_companies | social | global | 1 | – | ❌ strict-phrase, 2/4 |
| 80 | quora_companies | social | global+ | 1 | – | ❌ strict-phrase, 1/4 |
| 81 | amazon_sellers | ecommerce | global+ | 2 | – | ✅ |
| 82 | flipkart_sellers | ecommerce | india | 1 | – | ✅ |
| 83 | shopify_stores | ecommerce | global | 1 | – | ✅ |
| 84 | generic_companies_list | directory | global | 2 | – | ⚠️ generic |
| 85 | generic_association | directory | global+ | 2 | – | ⚠️ generic |
| 86 | generic_awards | directory | global | 2 | – | ⚠️ generic |
| 87 | generic_expo | directory | global+ | 1 | – | ⚠️ generic |
| 88 | generic_incubator | startup | global+ | 1 | – | ✅ |
| 89 | generic_govt_tender | government | india | 1 | – | ⚠️ generic |
| 90 | generic_iso_certified | directory | global+ | 1 | – | ⚠️ generic |
| 91 | generic_hiring_surge | jobs | global | 1 | – | ⚠️ generic |

**Coverage after the search fix (this session): 91/91 return data** (multi-phrase
validated; weak ones still flaky per phrase). The ⚠️/❌ flags are about *quality of
results*, not reachability.

## B. Lead-discovery: Special scrapers (9) — `scrapers/`

| Module | Strategy/source | Engine | Dependency gap |
|---|---|---|---|
| `google_maps.py` | `maps` | Patchright headless browser | ❌ **patchright not installed** → returns [] |
| `google_search.py` | `web` (duckduckgo) | DDG text + news | – (now resilient via fix) |
| `linkedin.py` | `linkedin` | DDG `site:linkedin.com/company` | title-parse fragility |
| `job_boards.py` | `job_boards` | DDG (Naukri/Indeed/Foundit) | URL-slug parse fragility |
| `crunchbase.py` | (used by registry/strategy) | DDG `site:crunchbase.com/organization` | domain-guessing unreliable |
| `review_directories.py` | `review_sites` | DDG (Clutch/GoodFirms/G2/AmbitionBox) | hardcoded city inference |
| `web_directories.py` | `directories` | DDG (JustDial/Sulekha/IndiaMart) + optional page scrape | needs patchright+bs4 for deep scrape |
| `csv_import.py` | import | local CSV (pandas) | manual curation |
| `data_collector_import.py` | import | Brazil CNPJ + GitHub CSVs (streaming, checkpointed) | Brazil-only; no cross-batch dedup |

## C. Native API source — AmbitionBox

`services/leadgen/ambitionbox.py` (`AmbitionBoxClient`) + `routers/ambitionbox.py`.
Reverse-engineered internal gateway (`servicegateway-ambitionbox`, headers
`appid=125/systemid=local`, no auth). Real structured search: industry/location/
rating filters, jobs, details, similar-companies. **Highest-quality India source**
(review-backed = actively employing). Risk: unofficial API may break.

## D. Person research — `services/person_intel.py`

Discovery+enrichment hybrid: from a LinkedIn URL, runs 7 categorized DDG-Lite
searches + GitHub API, extracts name/headline/emails/socials/articles/skills.
Used by the person-intel WS endpoint.

## E. Enrichment providers (26) — `enrichment/providers/`

Registered in `services/workbook/providers.py`; chained by `DEFAULT_WATERFALLS`
in **`services/workbook/enrichment.py`** (16 target fields).

- **Email find (7):** hunter_io*, snovio*, prospeo*, people_data_labs*, apollo_io*, email_harvester, ddg_email
- **Email verify (4):** mailscout, holehe, abstract_api*, debounce*
- **Phone (4):** numverify*, google_maps*, facebook_pages, local_business
- **Company intel (4):** ddg_company, company_intel, website_scraper, deep_scraper
- **People/decision-makers (2):** crosslinked, decision_maker
- **Signals/tech (3):** jobspy (hiring), tech_stack, social_finder
- **Scoring/util (2):** lead_scorer, ipinfo*

`*` = BYOK (env keys: `HUNTER_API_KEY`, `APOLLO_API_KEY`, `SNOVIO_CLIENT_ID/SECRET`,
`PROSPEO_API_KEY`, `PDL_API_KEY`, `ABSTRACT_API_KEY`, `DEBOUNCE_API_KEY`,
`NUMVERIFY_API_KEY`, `IPINFO_TOKEN`, `GOOGLE_MAPS_API_KEY`). 16 are free/OSS.

## F. Dead stubs — `apps/api/sources/legacy.py`

`SlideShareSource`, `ArchiveSource` (TODO: archive.org API), `GoogleBooksSource`
(TODO: Google Books API) — all return `[]`. Implement or delete.

---

# PART 2 — ENHANCEMENT PLAN

## ✅ Already done this session
Multi-engine + proxy→direct fallback search (`proxy_client.py`) — took registry
coverage 26/89 → 91/91 and removed the dead-proxy stalls. **Every source below
inherits this.** So the remaining work is *quality* and *config*, not reachability.

## Cross-cutting (do once — helps everything)

| # | Action | Files | Why |
|---|---|---|---|
| X1 | **Fix config gaps** | `settings.py`, `source_registry.py` | `yellowpages_in` in UI has no registry def (can't run) → add def or remove. `mouthshut_biz` registry def not in UI + mislabelled "India Yellow Pages" → surface + relabel `review`. |
| X2 | **Per-source registry toggles** | `settings.py` `get_source_enabled`, `job_runner._search_registry_sources` | Registry sources are all-or-nothing behind the `directories` toggle. Let each be enabled/disabled individually (the UI already lists them). |
| X3 | **Result-quality filter** | `job_runner._search_registry_sources` extraction | Drop ads / `.swf` / off-geo / pagination junk (indiamart, monsterindia, tradeindia, grotal). Add a denylist of asset/ad URL patterns + a geo check. |
| X4 | **Raise the top-N cap (visibly)** | `job_runner._search_registry_sources:1393` | Only top-20 by priority run per job, silently. Make it configurable and `log()` what was skipped. |
| X5 | **Install browser tier** | env | `pip install patchright curl_cffi` → enables `google_maps` + stronger anti-bot scrapes. |

## Registry-source template upgrades

| Source(s) | Problem | Fix |
|---|---|---|
| **forbes, quora_companies, reddit_companies** | strict exact-phrase `"best {industry} companies"` → low recall (1–2/4 phrases) | drop the quotes: `site:forbes.com {industry} companies {city}` etc. (broaden) |
| **indiamart, tradeindia** | bare `{q}` / supplier listings pull ads + assets | add manufacturer/company scoping, drop the bare-`{q}` template, apply X3 filter |
| **monsterindia, naukri, shine** | job boards mined as company dirs → stale/dupe | keep but tag as *hiring-signal* sources, not primary company data |
| **grotal** | ad-injected directory | apply X3 junk filter; lower priority |
| **single-template sources (~70)** | 1 template = low recall | add a 2nd/3rd template variant (with/without `{city}`, with company synonyms) to widen coverage |
| **generic_\*** (8) | no `site:` scope → broad | keep as last-resort (high priority number already); tighten with quoted industry + dedup |

## Listing-scrape sources (extract_from_listing=True)

`clutch, goodfirms, g2, softwaresuggest, capterra, getapp` set the flag, but the
registry path only reads DDG result URLs — **verify the listing-page card
extractor actually runs** for these (otherwise the flag is a no-op). Action: wire
`_search_registry_sources` to fetch+parse the listing page (reuse
`web_directories`/`review_directories` card logic) when `extract_from_listing`.

## Special-scraper upgrades

- **google_maps:** install patchright (X5); add an API fallback via the existing `google_maps` *provider* (Places API) when the browser tier is absent.
- **linkedin / job_boards / crunchbase / web/review_directories:** title-parse is fragile — centralize a `clean_company_title()` helper and reuse; add the X3 junk filter.
- **High-value API upgrades** (Clay-parity WI-8): replace `site:` scraping with real APIs for Crunchbase / LinkedIn / Capterra where a key is available.

## Enrichment-provider upgrades

| # | Action | Scope |
|---|---|---|
| P1 | **Normalize the result contract** — `numverify`/`ipinfo` return `data=` not `fields=`; standardize on `fields=` | 2 providers |
| P2 | **Per-provider timeouts** — `holehe` (120 site checks) and `deep_scraper` (8 pages) can stall a waterfall | provider base + 2 providers |
| P3 | **Confidence-based merge** instead of first-success in `WaterfallEnricher` — keep the highest-confidence value across providers | `enrichment/provider.py` |
| P4 | **Per-workspace BYOK keys** (ties to tenancy WI-6) — read provider keys from `workspace_settings`, not global | provider key loaders |
| P5 | **Tighten regex extractors** (email/phone/funding) — high false-positive risk in `company_intel`, `local_business`, `email_harvester` | ~5 providers |

## Dead stubs
Implement (`ArchiveSource` → archive.org API, `GoogleBooksSource` → Books API,
`SlideShareSource`) **or delete** `apps/api/sources/legacy.py` to stop them
showing as no-op sources.

## Prioritized roadmap

```
P1 (cheap, high-impact, ~1 day)
  X1 config gaps · X5 install patchright · weak-template rewrites
  (forbes/quora/reddit) · X3 junk filter

P2 (quality, ~2-3 days)
  X2 per-source toggles · verify listing-scrape sources · X4 visible cap
  · provider field-naming (P1) + timeouts (P2) · centralize title cleaner

P3 (depth, ~1 wk+)
  API upgrades for blockers (Crunchbase/LinkedIn/Capterra) · confidence-merge
  (P3) · per-workspace keys (P4) · implement/remove legacy stubs · add sources
```

**Smallest valuable slice:** P1 — it fixes the two config bugs, enables
`google_maps`, and de-noises the worst offenders, with no architectural change.
