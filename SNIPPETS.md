# Code Snippets Library — Verified Patterns from Researched Repos

> Every snippet below is extracted directly from the source repos.
> AI agents: use these as-is. Do not hallucinate APIs.

---

## Installation

```bash
# Core stack
pip install scrapling[all] cloakbrowser curl-cffi stealth-requests firecrawl-py

# Optional
pip install patchright pydoll primp
```

---

## 1. Stealth HTTP Requests (Tier 2)

### stealth-requests — Simplest API

```python
# Source: Stealth-Requests/stealth_requests/session.py
from stealth_requests import get

# Auto Chrome impersonation, UA rotation, Referer tracking
resp = get("https://example.com")

# Built-in extractors (no parsing needed)
resp.emails           # tuple of emails found in page
resp.phone_numbers    # tuple of phone numbers
resp.links            # tuple of all links
resp.images           # tuple of image URLs
resp.meta.title       # page title
resp.meta.description # meta description
resp.tables           # list of dicts from HTML tables

# Convert to markdown (for LLM processing)
resp.markdown()                              # full page
resp.markdown(content_xpath="//article")     # specific section

# Parse with lxml or BeautifulSoup
resp.tree()           # lxml HtmlElement
resp.soup()           # BeautifulSoup object
resp.xpath("//h1")    # direct xpath

# Session with retry logic
from stealth_requests.session import StealthSession

with StealthSession() as session:
    # Retries on 429, 503, 522 with 2s delay
    r = session.get("https://target.com", retry=3)

# Async version
from stealth_requests.session import AsyncStealthSession

async with AsyncStealthSession() as session:
    r = await session.get("https://target.com", retry=2)

# Retryable status codes (built-in):
# 408, 429, 500, 502, 503, 504, 520, 521, 522, 523, 524
```

### curl_cffi — Low-Level Fingerprint Control

```python
# Source: curl_cffi/examples/impersonate.py
from curl_cffi.requests import Session

# Simple impersonation
with Session(impersonate="chrome136") as s:
    r = s.get("https://target.com")

# With proxy
with Session(impersonate="chrome136") as s:
    r = s.get("https://target.com", proxy="socks5://user:pass@proxy:1080")

# Custom TLS fingerprint (JA3)
import curl_cffi
r = curl_cffi.get(url,
    ja3="771,4865-4866-4867-49195...",
    akamai="4:16777216|16711681|0|m,p,a,s",
    extra_fp={
        "tls_signature_algorithms": ["ecdsa_secp256r1_sha256", ...],
        # tls_min_version, tls_grease, tls_permute_extensions,
        # tls_cert_compression, http2_stream_weight, etc.
    }
)

# Async session
from curl_cffi.requests import AsyncSession

async with AsyncSession(impersonate="chrome136") as s:
    r = await s.get("https://target.com")
```

---

## 2. Browser Stealth (Tier 3)

### CloakBrowser — Modified Chromium

```python
# Source: CloakBrowser/examples/stealth_test.py
from cloakbrowser import launch, launch_async

# Sync
browser = launch(headless=True, proxy="socks5://proxy:1080", geoip=True)
page = browser.new_page()
page.goto("https://protected-site.com")
content = page.content()
browser.close()

# Async
browser = await launch_async(
    headless=True,
    args=["--remote-debugging-port=9245"]
)
page = browser.new_page()
await page.goto("https://protected-site.com")
await browser.close()

# Features: humanize=True for mouse curves, keyboard timing
browser = launch(headless=False, humanize=True)
```

### CloakBrowser + Scrapling (combined stealth + adaptive parsing)

```python
# Source: CloakBrowser/examples/integrations/scrapling_example.py
import asyncio, json
from urllib.request import urlopen
from cloakbrowser import launch_async
from scrapling.fetchers import StealthyFetcher

async def scrape(url):
    browser = await launch_async(
        headless=True,
        args=["--remote-debugging-port=9245",
              "--remote-debugging-address=127.0.0.1"]
    )
    info = json.loads(urlopen("http://127.0.0.1:9245/json/version").read())
    ws_url = info["webSocketDebuggerUrl"]

    page = await StealthyFetcher.async_fetch(url, cdp_url=ws_url)
    title = page.css("title::text").get()
    links = page.css("a::attr(href)").getall()
    await browser.close()
    return title, links
```

### Scrapling — Standalone Fetchers

```python
# Source: Scrapling/agent-skill/examples/
from scrapling.fetchers import Fetcher, AsyncFetcher, StealthyFetcher, DynamicFetcher

# Tier 2: HTTP-only fetcher (no browser)
with FetcherSession(impersonate="chrome") as session:
    page = session.get(url, stealthy_headers=True)
    quotes = page.css(".quote .text::text").getall()

# Tier 3: Stealthy browser fetcher
page = StealthyFetcher.fetch(url, headless=True, network_idle=True)
StealthyFetcher.adaptive = True  # auto-relocate elements on page changes

# Tier 3: Dynamic fetcher (heavy JS pages)
page = DynamicFetcher.fetch(url, headless=True)

# CSS selectors (same API for all fetchers)
page.css("h1::text").get()           # first match text
page.css("a::attr(href)").getall()   # all href values
page.css(".product .price::text").getall()
```

### patchright — Patched Playwright

```python
# Drop-in Playwright replacement
from patchright.sync_api import sync_playwright

with sync_playwright() as p:
    browser = p.chromium.launch(headless=True)
    page = browser.new_page()
    page.goto("https://protected-site.com")
    content = page.content()
    browser.close()
```

---

## 3. Data Extraction (Tier 4)

### Firecrawl — HTML to Structured Data

```python
# Source: firecrawl/examples/
from firecrawl import FirecrawlApp

app = FirecrawlApp(api_key="...")  # or self-hosted

# Scrape single URL to markdown
result = app.scrape_url("https://company.com", params={
    "formats": ["markdown"]
})
print(result["markdown"])

# Extract specific attributes
result = app.scrape_url("https://news.ycombinator.com", {
    "formats": [{
        "type": "attributes",
        "selectors": [
            {"selector": ".athing", "attribute": "id"}
        ]
    }]
})

# Crawl entire domain
result = app.crawl_url("https://company.com", params={
    "limit": 50,
    "formats": ["markdown"]
})

# Lead enrichment pattern (with LLM)
markdown = app.scrape_url(url, params={"formats": ["markdown"]})["markdown"]
# Feed to OpenAI/local LLM for structured extraction
```

### Firecrawl CRM Lead Enrichment (full pattern)

```python
# Source: firecrawl/examples/crm_lead_enrichment/crm_lead_enrichment.py
from firecrawl import FirecrawlApp
from openai import OpenAI

def enrich_lead(url: str) -> dict:
    firecrawl = FirecrawlApp(api_key="...")
    openai_client = OpenAI(api_key="...")

    # Step 1: Get clean markdown from website
    scraped = firecrawl.scrape_url(url, params={"formats": ["markdown"]})

    # Step 2: Extract structured data via LLM
    prompt = f"""
    Extract from this markdown:
    {{
        "is_open_source": boolean,
        "value_proposition": "string",
        "main_product": "string",
        "employee_count": "string",
        "tech_stack": ["string"]
    }}

    Content: {scraped["markdown"]}
    """
    completion = openai_client.chat.completions.create(
        model="gpt-4o",
        messages=[{"role": "user", "content": prompt}]
    )
    return json.loads(completion.choices[0].message.content)
```

---

## 4. Direct Discovery Tools (Tier 1)

### google-maps-scraper — Lead Fields Available

```
# Run as Docker service
docker run -p 8080:8080 gosom/google-maps-scraper

# Lead data returned per business:
title, categories[], address, phone, web_site, emails[],
review_count, review_rating, reviews_per_rating{},
latitude, longitude, status, open_hours{},
popular_times{}, price_range, description,
owner{name, id}, complete_address{borough, city, state, zip, country},
images[], reservations[], order_online[], menu
```

### openserp — SERP Results

```
# Run as Docker service  
docker run -p 7000:7000 karust/openserp

# Endpoints:
GET /google?q=roofing+Austin+TX&num=100
GET /bing?q=roofing+Austin+TX
GET /duckduckgo?q=roofing+Austin+TX
GET /yandex?q=roofing+Austin+TX
GET /baidu?q=roofing+Austin+TX
```

---

## 5. Proxy Sources

```python
# fresh-proxy-list repo — auto-updated files:
# http.txt    — HTTP proxies   (ip:port per line)
# https.txt   — HTTPS proxies
# socks4.txt  — SOCKS4 proxies
# socks5.txt  — SOCKS5 proxies

# Load proxies
import random

def load_proxies(type="socks5"):
    with open(f"research/fresh-proxy-list/{type}.txt") as f:
        return [line.strip() for line in f if line.strip()]

proxy = random.choice(load_proxies("socks5"))
# Use as: f"socks5://{proxy}"
```
