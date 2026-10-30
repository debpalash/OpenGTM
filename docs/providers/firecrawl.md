# Firecrawl search in the Go backend

This is the native Go port of the opt-in search behavior proposed in
[PR #116](https://github.com/debpalash/OpenGTM/pull/116). With no key configured,
Firecrawl makes no requests and existing search behavior remains unchanged.

```dotenv
FIRECRAWL_API_KEY=your-firecrawl-key
SEARCH_ENGINE_TIMEOUT=10
# Optional explicit keyed order (none disables all keyed search engines):
# SEARCH_FALLBACK_ORDER=firecrawl,brave
```

Restart the API and worker after changing the server environment. The default
keyed order is `serpapi,bing,google_cse,brave,serpingapi,serply,firecrawl`.
Keyless results win first; news never invokes this chain. API-only search also
works without the optional browser sidecar. Keys are operator-managed and its
provider account pays for searches from its workspaces.

The fixed endpoint is `https://api.firecrawl.dev/v2/search`. The query goes in a
JSON POST body and the key only in a bearer authorization header. Requests ask
for web results, plain snippets (`highlights: false`), and no scraped content.
Queries are limited to 500 Unicode characters, result counts to 100. Results
map to the shared title/URL/snippet shape. See the
[official API contract](https://docs.firecrawl.dev/api-reference/endpoint/search).

## Security and behavior

- Preserve the strict egress transport and reject every API redirect, including
  request-body replays on 307/308 responses.
- Require a successful API envelope and one complete JSON document within a
  2 MiB decompressed response cap. Invalid responses yield no hits so another
  configured fallback can answer.
- Reject non-HTTP(S), missing-host, credential-bearing, control-character,
  backslash and syntactically private result URLs. Search hits are not fetched
  here; actual page reads must also enforce DNS and redirect policy.
- Respect caller cancellation and a finite configured deadline. Keys, queries
  and provider error bodies are absent from returned errors/logs; client
  formatting redacts credentials.
- Cache results separately per workspace and fingerprint provider credentials
  when choosing a shared process client.

HTTP fixture tests cover request shape, result and query limits, malformed and
oversized responses, redirects, cancellation, redaction, keyless-first order,
news behavior and workspace cache isolation. They do not call paid production
endpoints. Worker tests cover API-only configuration and credential-specific
client reuse.
