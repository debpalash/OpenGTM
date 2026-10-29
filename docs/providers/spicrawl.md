# Spicrawl markdown and PDFs

Spicrawl is OpenGTM's strategic sponsor and recommended optional research page
provider. [Create a free account](https://app.spicrawl.com/signup?utm_source=opengtm&utm_medium=docs&utm_campaign=strategic_sponsor).
Its [published pricing](https://spicrawl.com/pricing) currently offers 1,000
credits per month during beta; availability and limits follow its published
terms.

```dotenv
SPICRAWL_API_KEY=your-spicrawl-key
SPICRAWL_TIMEOUT=60
```

Restart the API and worker after setting the server environment. The key is
operator-managed and its account pays for requests made by its workspaces.
With a keyed search engine (such as Firecrawl) also configured, research can run
without the optional browser sidecar. Without a Spicrawl key, page reads keep
using the existing browser pipeline.

When `SPICRAWL_API_KEY` is set, research page reads use its fixed `/v1/scrape`
endpoint. Requests explicitly ask for markdown, main content only, PDF parsing,
no link extraction, and no JavaScript rendering. Research previews are capped
at 2,000 Unicode characters. The client also exposes `Scrape` for bounded full
markdown, `IncludeTags`/`ExcludeTags`, explicit `JSRender`, target status and
credits charged. See [Spicrawl's markdown guide](https://docs.spicrawl.com/guides/markdown).

Each default request sends `max_cost: 1`; explicit JavaScript mode sends
`max_cost: 3`. There are no automatic paid retries or JavaScript escalations.
Repeated successful calls, including provider cache hits, can still cost
credits. This provider spend is separate from the LLM token budget.

The client checks target URL syntax and resolves DNS before sending a public
URL to the provider; any private/reserved result blocks the request. Userinfo
is rejected and fragment-only tokens are removed. Target URL queries are sent
to Spicrawl, so configure this provider only for content that may be shared
with that service. Local preflight cannot pin the remote provider's DNS or
redirect hops: those remain Spicrawl's responsibility.

An HTTP 200 from the platform is insufficient: `X-Target-Status` must be 2xx,
`X-Credits-Charged` must be a valid amount within the requested ceiling, and the
body must be nonempty UTF-8 markdown/plain text within 4 MiB. Missing metadata,
target errors, HTML/problem responses and coerced JSON envelopes fail closed.
The normal request explicitly overrides project defaults that could enable
link extraction. Platform redirects never forward the key or target URL.

## Validation

Local HTTP fixtures cover markdown/PDF response handling, strict target status
and credit headers, cost ceilings, explicit JavaScript/selector options, preview
limits, redirects, private and mixed DNS results, and malformed/oversized bodies.
They never call paid production endpoints. Worker tests cover markdown-only and
combined Firecrawl/Spicrawl configurations without a browser service, and ensure
different provider credentials cannot reuse a cached client.
