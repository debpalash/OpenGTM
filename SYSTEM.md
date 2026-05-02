# Lead Data Pipeline — Enterprise System Design

> This document is the single source of truth for AI agents building this platform.
> Read `research-reference.md` for tool catalog. Read `research-ai-context/` for code-level understanding.

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                        QUERY INTAKE                                 │
│  User submits: "roofing companies in Austin TX with websites"       │
│  → Query Parser → Job Queue (SQLite/Redis)                         │
└────────────────────────┬────────────────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────────┐
│                    ORCHESTRATOR (Python)                             │
│  Reads job, determines strategy, dispatches to correct tier         │
│  Manages retries, escalation, rate limits, proxy rotation           │
└──┬──────────┬──────────┬──────────┬─────────────────────────────────┘
   │          │          │          │
   ▼          ▼          ▼          ▼
┌──────┐ ┌──────┐ ┌──────────┐ ┌──────────┐
│Tier 1│ │Tier 2│ │ Tier 3   │ │ Tier 4   │
│Direct│ │HTTP  │ │ Browser  │ │ Extract  │
│APIs  │ │Stealth│ │ Stealth  │ │ & Store  │
└──────┘ └──────┘ └──────────┘ └──────────┘
   │          │          │          │
   └──────────┴──────────┴──────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      DATA STORE                                     │
│  SQLite (leads.db) → Structured leads with scores                  │
│  Raw HTML cache (filesystem) → For re-processing                   │
└─────────────────────────────────────────────────────────────────────┘
```

---

## Tier Details & Tool Mapping

### Tier 1 — Direct Discovery (0 evasion needed)

**When**: Query targets Google Maps, SERP results, or known API endpoints.

| Tool | Use Case | Invocation |
|------|----------|------------|
| `google-maps-scraper` | Local business leads with email/phone/website | Binary CLI or REST API (Go, runs as Docker service) |
| `openserp` | Google/Bing/DuckDuckGo SERP results | Docker service with REST API |
| `API-mega-list` | Check if a direct Apify/RapidAPI actor exists | Lookup database (markdown) |

**Data model from google-maps-scraper** (this is the gold standard for lead fields):
```
title, categories, address, phone, web_site, emails[],
review_count, review_rating, latitude, longitude,
status, open_hours, popular_times, price_range,
owner, complete_address, images[], reservations[]
```

### Tier 2 — HTTP Stealth (lightweight, no browser)

**When**: Need to scrape websites directly, no heavy JS rendering required.

| Tool | Use Case | Pattern |
|------|----------|---------|
| `curl_cffi` | Core HTTP engine. Impersonates Chrome TLS/HTTP2 fingerprint | `curl_cffi.get(url, impersonate="chrome136")` |
| `Stealth-Requests` | Higher-level wrapper. Auto UA rotation, Referer tracking, retry on 429/503/522 | `StealthSession().get(url, retry=3)` |
| `spider` / `crawley` | Deep domain crawl to discover all URLs | Rust/Go binaries, pipe URL list |
| `primp` / `wreq-python` | Alternative HTTP impersonation (Rust-powered) | For when curl_cffi is detected |

**Stealth-Requests session pattern** (use this as default):
```python
from stealth_requests import get

# Simple — auto-impersonates Chrome, rotates UA, tracks Referer
resp = get("https://example.com")
print(resp.emails)          # Extracted emails
print(resp.phone_numbers)   # Extracted phones
print(resp.meta.title)      # Page metadata
print(resp.links)           # All links
resp.markdown()             # Convert to markdown
```

**curl_cffi for custom fingerprints**:
```python
from curl_cffi.requests import Session

with Session(impersonate="chrome136") as s:
    r = s.get("https://target.com", proxy="socks5://proxy:1080")
    # Full TLS fingerprint control via ja3=, akamai=, extra_fp=
```

### Tier 3 — Browser Stealth (heavy, JS rendering + anti-bot bypass)

**When**: Tier 2 returns 403, Cloudflare challenge page, or JS-rendered content.

| Tool | Use Case | Pattern |
|------|----------|---------|
| `CloakBrowser` | Modified Chromium binary (49 C++ patches). Passes Turnstile, reCAPTCHA 0.9 | `from cloakbrowser import launch` |
| `Scrapling.StealthyFetcher` | Manages browser sessions with adaptive CSS parsing | `StealthyFetcher.fetch(url, headless=True)` |
| `patchright` | Patched Playwright — drop-in replacement | `from patchright.sync_api import sync_playwright` |
| `camofox-browser` | Firefox-based anti-detection (Camoufox) | Docker service, Playwright-compatible |
| `pydoll` | Async browser automation, evasion-focused | CDP-based, Python 3.10+ |

**CloakBrowser + Scrapling integration** (the nuclear option):
```python
from cloakbrowser import launch_async
from scrapling.fetchers import StealthyFetcher
import json, asyncio
from urllib.request import urlopen

async def scrape_protected(url):
    browser = await launch_async(
        headless=True,
        args=["--remote-debugging-port=9245"]
    )
    info = json.loads(urlopen("http://127.0.0.1:9245/json/version").read())
    ws_url = info["webSocketDebuggerUrl"]

    page = await StealthyFetcher.async_fetch(url, cdp_url=ws_url)
    data = page.css("h1::text").get()
    await browser.close()
    return data
```

### Tier 4 — Extraction & Structuring

**When**: Raw HTML/markdown is retrieved, need structured data for the database.

| Tool | Use Case | Pattern |
|------|----------|---------|
| `firecrawl` | HTML → clean Markdown → LLM extraction | Self-hosted or API |
| `Stealth-Requests` | Built-in email/phone/link/table extraction | `resp.emails`, `resp.tables` |
| `maxun` | Visual no-code extraction for complex pages | Docker, web UI |
| LLM (OpenAI/local) | Schema-based entity extraction from markdown | JSON mode with Pydantic schema |

**Firecrawl lead enrichment pattern**:
```python
from firecrawl import FirecrawlApp

app = FirecrawlApp(api_key="...")  # or self-hosted
result = app.scrape_url("https://company.com", params={"formats": ["markdown"]})
# Feed result["markdown"] to LLM for structured extraction
```

---

## Enterprise Components

### 1. Job Queue & Orchestrator

```python
# jobs table schema
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    query TEXT NOT NULL,              -- "roofing Austin TX"
    status TEXT DEFAULT 'pending',    -- pending|running|tier2|tier3|done|failed
    tier INTEGER DEFAULT 1,
    attempts INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 3,
    proxy_used TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP,
    error TEXT
);

# leads table schema
CREATE TABLE leads (
    id TEXT PRIMARY KEY,
    job_id TEXT REFERENCES jobs(id),
    title TEXT,
    category TEXT,
    address TEXT,
    city TEXT,
    state TEXT,
    phone TEXT,
    email TEXT,
    website TEXT,
    rating REAL,
    review_count INTEGER,
    latitude REAL,
    longitude REAL,
    source TEXT,                      -- "gmaps"|"serp"|"website_scrape"
    raw_html_path TEXT,              -- filesystem cache
    enrichment_json TEXT,            -- LLM-extracted metadata
    score REAL,                      -- lead quality score
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### 2. Proxy Pool Manager

```python
# Proxy rotation strategy
class ProxyPool:
    """
    Sources:
    - fresh-proxy-list repo (auto-updated txt files: http.txt, socks5.txt)
    - free-proxy-list repo (databay.com API)
    - Custom paid proxies (residential)

    Health check: ping each proxy every 5min, remove dead ones.
    Domain bucketing: assign proxy groups per target domain to avoid
    pattern detection (same proxy always hits same domain).
    """

    def get_proxy(self, domain: str, tier: int) -> str:
        # Tier 1-2: free proxies are fine
        # Tier 3: use residential proxies for browser sessions
        ...

    def report_blocked(self, proxy: str, domain: str):
        # Cooldown this proxy for this domain for 30 minutes
        ...
```

### 3. Rate Limiter & Circuit Breaker

```python
# Per-domain rate limiting
RATE_LIMITS = {
    "default": {"requests_per_minute": 10, "concurrent": 2},
    "google.com": {"requests_per_minute": 3, "concurrent": 1},
    "linkedin.com": {"requests_per_minute": 2, "concurrent": 1},
    "yelp.com": {"requests_per_minute": 5, "concurrent": 2},
}

# Circuit breaker: if 5 consecutive failures on a domain, pause for 15 minutes
CIRCUIT_BREAKER = {
    "failure_threshold": 5,
    "recovery_timeout_seconds": 900,
}
```

### 4. Tier Escalation Logic

```python
async def execute_job(job):
    """
    Escalation flow:
    1. Try Tier 1 (direct API) if query matches known patterns
    2. Try Tier 2 (HTTP stealth) — curl_cffi/stealth-requests
    3. If 403/challenge detected → escalate to Tier 3 (browser)
    4. Extract data with Tier 4 regardless of which tier fetched HTML
    """
    url = determine_target_url(job.query)

    # Tier 1: Direct APIs
    if is_maps_query(job.query):
        return await gmaps_scraper(job.query)
    if is_serp_query(job.query):
        return await openserp_search(job.query)

    # Tier 2: HTTP stealth
    proxy = proxy_pool.get_proxy(urlparse(url).netloc, tier=2)
    resp = stealth_requests.get(url, proxy=proxy, retry=2)

    if resp.status_code == 200 and not is_challenge_page(resp.text):
        return await extract_leads(resp)

    # Tier 3: Browser escalation
    job.tier = 3
    proxy = proxy_pool.get_proxy(urlparse(url).netloc, tier=3)
    page = await scrape_with_cloakbrowser(url, proxy=proxy)
    return await extract_leads(page)
```

### 5. Data Enrichment Pipeline

```python
async def extract_leads(html_or_page) -> list[Lead]:
    """
    Multi-pass extraction:
    1. Direct extraction (emails, phones from HTML regex)
    2. Firecrawl markdown conversion
    3. LLM structured extraction with schema
    4. Deduplication against existing leads
    5. Quality scoring
    """
    # Pass 1: regex extraction
    emails = extract_emails(html)
    phones = extract_phones(html)

    # Pass 2: convert to clean markdown
    markdown = firecrawl.scrape_url(url, formats=["markdown"])

    # Pass 3: LLM extraction
    schema = {
        "company_name": "string",
        "industry": "string",
        "employee_count": "string",
        "decision_maker": "string",
        "tech_stack": ["string"],
    }
    enriched = await llm_extract(markdown, schema)

    # Pass 4: deduplicate
    # Pass 5: score (has email? has phone? has website? review count?)
    lead = Lead(**enriched, emails=emails, phones=phones)
    lead.score = calculate_score(lead)
    return lead
```

### 6. Monitoring & Observability

```python
# Metrics to track
METRICS = {
    "jobs_completed_total": Counter,
    "jobs_failed_total": Counter,
    "tier_escalations": Counter,          # How often Tier 2 → 3
    "proxy_blocks": Counter,              # Per proxy, per domain
    "avg_extraction_time_seconds": Histogram,
    "leads_per_job": Histogram,
    "cost_per_lead": Gauge,               # Proxy cost + API cost
    "active_browser_sessions": Gauge,
}
```

---

## File Structure

```
lead-data/
├── SYSTEM.md                    ← You are here (architecture reference)
├── research-reference.md        ← Tool catalog (30 repos, categorized)
├── research-ai-context/         ← Code-level understanding per repo
│   ├── CloakBrowser.ai.txt
│   ├── Scrapling.ai.txt
│   ├── curl_cffi.ai.txt
│   ├── firecrawl.ai.txt
│   └── ... (30 files, ~2MB total)
├── research/                    ← Full repo clones (gitignored)
├── research-summaries/          ← Full repomix dumps (for deep dives)
├── src/                         ← Backend source code
│   ├── orchestrator.py          ← Job queue + tier escalation
│   ├── tiers/
│   │   ├── tier1_apis.py        ← gmaps, openserp wrappers
│   │   ├── tier2_http.py        ← stealth-requests, curl_cffi
│   │   ├── tier3_browser.py     ← CloakBrowser, Scrapling
│   │   └── tier4_extract.py     ← firecrawl, LLM extraction
│   ├── proxy_pool.py            ← Proxy management
│   ├── rate_limiter.py          ← Per-domain rate limiting
│   ├── models.py                ← Pydantic schemas (Lead, Job)
│   └── db.py                    ← SQLite operations
├── data/
│   └── leads.db                 ← SQLite database
└── cli.py                       ← CLI entry point
```

---

## Decision Rules for AI Agents

1. **Always try the cheapest tier first.** Never launch a browser when HTTP works.
2. **Detect challenge pages** by checking for `<title>Just a moment</title>`, `cf-challenge`, `__cf_bm` cookie, or response body containing `turnstile`.
3. **Rotate proxies per domain**, not per request. Same proxy → same domain builds trust.
4. **Cache raw HTML** to filesystem. Never re-scrape what you already have.
5. **Rate limit aggressively.** 3 req/min for Google, 2 req/min for LinkedIn, 10 req/min default.
6. **Use async everywhere.** `asyncio` + `curl_cffi.AsyncSession` + `launch_async` for browsers.
7. **Dedup by (domain + phone) or (domain + email).** Don't store duplicate leads.
8. **Score leads**: email=+30, phone=+20, website=+10, reviews>10=+15, rating>4=+10.
9. **Prefer Python packages**: `pip install scrapling[all] cloakbrowser curl-cffi stealth-requests firecrawl-py`.
10. **For Go tools** (gmaps-scraper, openserp): run as Docker sidecar services with REST APIs.
