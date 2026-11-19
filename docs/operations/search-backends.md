# Web search backends

Search-driven enrichment (web_search, the sourcing scrapers, search-based providers) picks a backend in this order:

1. A SearXNG pool (`SEARXPOOL_URL`), then a direct SearXNG (`SEARXNG_URL`), when one is reachable.
2. Keyless search through the browser service (ddgs fans a query out to `BROWSER_SEARCH_BACKENDS`, default
   `duckduckgo, google, bing, brave, mojeek, yahoo`).
3. Keyed engines, in `SEARCH_FALLBACK_ORDER` (SerpAPI, Bing, Google CSE, Brave, Serping, Serply, Firecrawl, Keenable),
   used only when a key is configured.

## What to expect from keyless search

The keyless backends are public search pages. From a server or datacentre address they are routinely blocked: Brave
answers 429, Mojeek 403, DuckDuckGo 202 or a timeout. That is the engines' policy, not a bug here, and the product does
not try to get around it (no fingerprint spoofing, no proxy rotation to defeat a block).

**The honest answer is your own search API key or your own SearXNG instance.** Either one makes search reliable and
is the only thing that fixes blocked keyless search:

- Set one of the keys in `.env` (`SERPAPI_KEY`, `BRAVE_SEARCH_KEY`, `BING_SEARCH_KEY`, `GOOGLE_CSE_KEY` with `GOOGLE_CSE_ID`,
  `SERPINGAPI_API_KEY`, `SERPLY_API_KEY`, ...). Settings shows which are configured.
- Or run SearXNG and set `SEARXNG_URL` (or `SEARXPOOL_URL` for a pool).

## How the keyless path behaves when a backend says no

- The browser service remembers what each backend last answered (`scrapers/backend_health.py`). A backend that
  answered 429, 403, 202 or 503 is not asked again for 5 minutes (doubling up to 1 hour while it keeps refusing);
  one that timed out or failed is left alone for 1 minute (doubling up to 10 minutes). A 200 clears it. Before this, a
  fresh engine set asked every backend on every query, so a backend that had just said 429 was asked again at once and
  a dead backend cost a full search timeout (10 s) per query.
- When every configured backend is cooling down, the query fails at once as `rate_limited` without a single request,
  and the Go side grows its own keyless backoff (`ddgs` key) so keyed fallbacks run next.
- `WebSearch` (used by providers) shares that Go backoff with the sourcing search: a blocked answer grows it, a
  cooling backoff skips the keyless call, and an answer clears it.
