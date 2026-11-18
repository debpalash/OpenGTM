# Keenable search in the Go backend

Keenable is an optional web-search fallback for lead web search. It is **off by
default**: with neither `KEENABLE_API_KEY` nor `KEENABLE_ENABLED=true` set, it
sends no request and existing search behavior is unchanged. Keenable also has a
keyless public tier, but OpenGTM never uses it implicitly; you must opt in.

## Enable

```dotenv
# Keyed (recommended): uses POST /v1/search, no hourly cap
KEENABLE_API_KEY=your-keenable-key

# Or keyless: uses POST /v1/search/public (rate limited per IP; Keenable documents
# 1,000 requests/hour/IP and no credits used). Off unless enabled here.
# KEENABLE_ENABLED=true

SEARCH_ENGINE_TIMEOUT=10
# Optional explicit order (none disables all keyed engines):
# SEARCH_FALLBACK_ORDER=keenable,brave
```

Restart the API and worker after changing the server environment. If both are
set, the key is used. A key alone enables the engine (the key is the opt-in).
`keenable` is last in the default fallback order
(`serpapi,bing,google_cse,brave,serpingapi,serply,firecrawl,keenable`); keyless
browser results win first, and news never invokes this chain.

## What is sent, and when

Only when enabled, only after keyless browser search returned nothing, and only
for lead-web text searches: one JSON POST containing the search query and the
result count (`{"query": "...", "max_results": N}`, N up to 50, queries up to
500 characters) to `https://api.keenable.ai/v1/search` (keyed) or
`https://api.keenable.ai/v1/search/public` (keyless). Nothing else about the
workspace, lead or user is sent.

- Keyed: the key goes only in an `Authorization: Bearer` header. Usage counts
  against your Keenable account (Keenable lists a free monthly tier; check their
  pricing for current limits).
- Keyless: no credential; a fixed `X-Keenable-Title: OpenGTM` app identifier
  header is sent (Keenable requires it). Search queries still leave your server
  for a third party, so only enable this if that is acceptable.

Results map to the shared title/URL/snippet shape. See the
[official API reference](https://docs.keenable.ai/api-reference/search).

## Security and behavior

- Strict egress client; API redirects are never followed.
- One JSON document within a 2 MiB response cap; any non-200 status (401, 402,
  403, 429, 5xx), malformed JSON or timeout yields no hits so another configured
  fallback can answer. Keys, queries and error bodies are never logged.
- Result URLs that are non-HTTP(S), credential-bearing, control-character or
  syntactically private are dropped.
- The deadline comes from `SEARCH_ENGINE_TIMEOUT`.

Unisearch (document sources) is intentionally not wired to Keenable: it serves
downloadable documents with a parity-tested source order, and Keenable returns
general web pages.
