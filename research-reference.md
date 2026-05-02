# Research Repos — Quick Reference

30 repos for lead gen pipeline: scraping, stealth browsers, proxies, and APIs.

---

## 🕵️ Stealth Browsers (anti-detection)

| Repo | Lang | What it does |
|------|------|-------------|
| **CloakBrowser** 🐳 | Python/JS | Real Chromium binary with 49 C++ source-level patches. Passes Cloudflare Turnstile, reCAPTCHA v3 (0.9 score). Drop-in Playwright replacement. `pip install cloakbrowser` |
| **patchright** | TypeScript | Patched Playwright driver that evades bot detection. Undetected automation. |
| **patchright-python** | Python | Python wrapper for Patchright. `pip install patchright` |
| **patchright-nodejs** | TypeScript | Node.js wrapper for Patchright. `npm install patchright` |
| **camofox-browser** 🐳 | JS | Anti-detection browser server for AI agents. Firefox-based (Camoufox) with C++ fingerprint spoofing. |
| **VirtualBrowser** | JS | Chromium-based fingerprint browser. Multiple isolated browser profiles on one machine. Windows-only. Chinese docs. |

## 🌐 Web Scraping Frameworks

| Repo | Lang | What it does |
|------|------|-------------|
| **Scrapling** 🐳 | Python | Adaptive scraping framework. Parser auto-relocates elements when pages change. Built-in Cloudflare bypass. Spiders with proxy rotation. |
| **firecrawl** 🐳 | TS/Python/Rust | Turn websites into LLM-ready data. Crawl, scrape, extract structured data. REST API. 1300+ files, production-grade. |
| **maxun** 🐳 | React/TS | No-code web scraping platform. Visual UI to turn any website into a structured API. Real-time extraction. |
| **webclaw** 🐳 | Rust | Fastest scraper for AI agents. 67% fewer tokens, sub-ms extraction, zero browser overhead. |
| **spider** | Rust | Fast web crawler. Concurrent, respects robots.txt. Cloud service available. |
| **crawley** | Go | Simple link crawler. Fast HTML SAX parser + JS/CSS lexical parsing. |
| **design-extract** | JS | Points headless browser at URL, reads design system from live DOM. Outputs tokens, Tailwind config, shadcn theme. |
| **scrapfly-scrapers** | Python | 40+ educational scrapers for popular sites (Amazon, TikTok, LinkedIn, etc.) using ScrapFly API. |

## 🔒 HTTP Stealth / TLS Fingerprinting

| Repo | Lang | What it does |
|------|------|-------------|
| **curl_cffi** | Python | Python binding for curl-impersonate. Mimics browser TLS/HTTP2 fingerprints. `pip install curl_cffi` |
| **Stealth-Requests** | Python | Easiest stealth HTTP client. Auto-rotates UAs, tracks Referer chains, retry logic. Built on curl_cffi. |
| **httpcloak** | Go/JS/Python | Every byte indistinguishable from Chrome. TLS, HTTP/2, QUIC fingerprint matching. |
| **primp** | Rust/Python | HTTP client that impersonates browsers. Rust core with Python bindings. |
| **wreq-python** | Python/Rust | Ergonomic async HTTP client with browser impersonation. Rust-powered. |
| **helium** | Python | High-level browser automation. Simplifies Selenium/Chrome interactions. |
| **pydoll** | Python | Async-native, fully typed browser automation. Built for evasion and performance. Python 3.10+. |

## 🔄 Proxies

| Repo | Lang | What it does |
|------|------|-------------|
| **free-proxy-list** | Data | Continuously updated proxy lists from databay.com. Browse/filter/download with API. |
| **fresh-proxy-list** | Data/JS | Auto-updated tested proxy server lists. HTTP/HTTPS/SOCKS4/SOCKS5. |

## 🛠️ Specialized Tools

| Repo | Lang | What it does |
|------|------|-------------|
| **google-maps-scraper** 🐳 | Go | Extract Google Maps leads: emails, phones, websites, ratings, coordinates. CLI + Web UI + REST API. |
| **openserp** 🐳 | Go | API/CLI for Google, Yandex, Baidu, Bing, DuckDuckGo SERP results. Free alternative to paid SERP APIs. |
| **WebAI2API** 🐳 | JS | Converts web AI services (LMArena, Gemini) to OpenAI-compatible API. Multi-window concurrent. Uses Camoufox. |
| **pinchtab** 🐳 | Go/React | Browser control for AI agents. HTTP API, token-efficient. Small Go binary. |
| **browserless** | JS | Headless Chrome/Chromium driver on top of Puppeteer. Screenshots, PDFs, HTML extraction. |
| **user-agents** | TS | User-agent string library. Parse and generate realistic UA strings. |
| **API-mega-list** | Data | 10,498 curated APIs across 18 categories (lead gen, social media, automation, SEO, etc.). |

---

## 🐳 = Has Docker support

## Key Takeaways for Lead Pipeline

1. **Scraping stack**: Scrapling (adaptive) or firecrawl (LLM-ready) for main scraping
2. **Stealth layer**: CloakBrowser or patchright for anti-detection browser sessions
3. **HTTP layer**: curl_cffi or primp for stealth HTTP requests without browser
4. **Lead extraction**: google-maps-scraper for GMB leads, openserp for SERP data
5. **Proxy rotation**: fresh-proxy-list for free proxies, Scrapling has built-in rotation
6. **AI integration**: webclaw for token-efficient extraction, WebAI2API for LLM access
