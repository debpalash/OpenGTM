# SpecStream search in the Go backend

SpecStream is an opt-in keyed search engine in the fallback chain, built the
same way as the [Firecrawl engine](firecrawl.md). With no key configured,
SpecStream makes no requests and existing search behavior remains unchanged.

```dotenv
SPECSTREAM_API_KEY=your-specstream-key
SEARCH_ENGINE_TIMEOUT=10
# Optional explicit keyed order (none disables all keyed search engines):
# SEARCH_FALLBACK_ORDER=specstream,brave
```

Restart the API and worker after changing the server environment. The default
keyed order is `serpapi,bing,google_cse,brave,serpingapi,serply,firecrawl,specstream`,
so SpecStream answers only when every engine before it is unset or returns
nothing. Keyless results win first; news never invokes this chain. API-only
search also works without the optional browser sidecar. Keys are
operator-managed and its provider account pays for searches from its workspaces.

The fixed endpoint is `https://api.specstream.ai/v1/web-search`. The request is
a GET with `q`, `num` and `speed=fast`; the key goes only in a bearer
authorization header, never in the URL. Queries are limited to 2000 Unicode
characters, result counts to 100. `sources[]` (`title` / `url` / `snippet`,
plus `published` when the page states a date) maps to the shared
title/URL/snippet/date shape. See the [API docs](https://specstream.ai/docs).

## Security and behavior

- Preserve the strict egress transport and reject every API redirect.
- Require a `request_id` in the response and one complete JSON document within
  a 2 MiB decompressed response cap. Non-200 statuses and invalid responses
  yield no hits so another configured fallback can answer.
- Reject non-HTTP(S), missing-host, credential-bearing, control-character,
  backslash and syntactically private result URLs. Search hits are not fetched
  here; actual page reads must also enforce DNS and redirect policy.
- Respect caller cancellation and a finite configured deadline. Keys, queries
  and provider error bodies are absent from returned errors/logs; client
  formatting redacts credentials.
- Cache results separately per workspace and fingerprint provider credentials
  when choosing a shared process client.

HTTP fixture tests cover request shape, result and query limits, a golden
response fixture (`internal/leadweb/testdata/specstream_search.json`), failed,
malformed and oversized responses, redirects, cancellation, redaction,
keyless-first order, news behavior and workspace cache isolation. They do not
call paid production endpoints.
