# Scavio in the Go backend

[Scavio](https://scavio.dev/google-maps-api) is a real-time
search data API. With no key configured it makes no request and existing
behavior is unchanged.

```dotenv
SCAVIO_API_KEY=your-scavio-key
```

A free account includes 50 credits; each Maps lookup costs one credit.

## `scavio_maps` enrichment provider

Looks a company up on Google Maps and fills the same fields as `google_maps`:
`phone`, `address`, `description`, `industry_tags`, `google_rating` and
`google_maps_url`. The query is the lead's company name plus its city. Maps
answers an unknown name with nearby places, so of the first three results the
first whose title contains the company's leading name word is used; if none
does, the lookup is a miss (`No matching place found`) rather than another
business's data. It needs no Google Cloud project.

Add `scavio_maps` to a column's waterfall (for example after `google_maps` on
`phone` or `address`). It is not part of any default waterfall. The key
resolves like `GOOGLE_MAPS_API_KEY`: a workspace setting, then the global
setting, then the server environment.

- Endpoint: `POST https://api.scavio.dev/api/v2/google/maps/search`, JSON body
  `{"query": "<company> <city>"}`, key in a bearer `Authorization` header.
- A non-200 answer is a miss with `Scavio API error: <status>`; an empty result
  is `No places found`. The key never appears in an error.
- A fresh Maps lookup can take longer than the default 10-second provider
  timeout. With `scavio_maps` in a waterfall, raise **Provider timeout (s)** under
  Settings > Enrichment Performance to 15 (the per-request ceiling every
  provider shares).

Restart the API and worker after changing the server environment.
