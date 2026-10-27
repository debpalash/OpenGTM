# Browser service (apps/browser-service)

The only Python left at runtime after plan 15. A small FastAPI app that does browser and anti-bot work and
nothing else: Playwright Chromium rendering, curl_cffi TLS impersonation, FlareSolverr, and the ddgs-based
search scrapers. It has no database, no Redis, no secrets store, no LLM calls and no scoring or dedup. Callers
send everything a request needs (cookies included) and get back raw data or HTML. Lead mapping, dedup,
caching, keyed search fallbacks and proxy-pool rotation run in Go.

Go reaches it only through `internal/browserclient`.

## Why FastAPI

- It generates the OpenAPI schema from the pydantic models. That schema is the contract the Go client is tested
  against (`apps/backend-go/internal/browserclient/testdata/openapi.json`).
- Pydantic validation keeps the request bounds (lengths, list sizes, timeouts) in one place.
- It is async-native, which Playwright's async API needs.
- It is already the stack the legacy code used, so the scrapers ported with almost no changes.

## API (`/v1`, bearer token required)

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Unauthenticated liveness check. Returns only `{"status":"ok"}`. |
| `GET /v1/health` | Browser state (ready/down/disabled), active requests, capacity, available engines. |
| `POST /v1/render` | url -> final_url, status, html and/or markdown-ish text, title, description, challenge flag, blocked_requests. `engine`: `browser` (default), `impersonate` (curl_cffi, no JS) or `flaresolverr`. Options: `wait_ms`, `scroll`, `cookies`, `max_bytes`. |
| `POST /v1/screenshot` | jpeg/png as base64. |
| `POST /v1/scrape/google_maps` | Maps places for query + city. |
| `POST /v1/scrape/justdial_page` | Listing cards from a justdial.com page. |
| `POST /v1/scrape/{linkedin,job_boards,crunchbase,review_directories,web_directories,google_search}` | Raw search hits per search string. The search strings are the exact legacy templates. |

Errors use the envelope `{"error":{"code","message","fields?"}}`. Each code has one fixed message, so page text
and library exception text never reach the caller. The codes are: `unauthorized` 401, `bad_request` 400,
`egress_blocked` 403, `too_large` 413, `busy` 429, `timeout` 504, `navigation_failed` / `upstream_error` 502,
`engine_unavailable` / `browser_unavailable` 503, `internal` 500.

## Limits (env)

| Variable | Default | Meaning |
|---|---|---|
| `BROWSER_SERVICE_TOKEN` | (required, >= 32 chars) | Shared bearer token, compared in constant time. The service exits with status 2 if it is missing. |
| `BROWSER_MAX_CONCURRENCY` | 4 | Concurrent browser/search requests. A request beyond this waits up to `BROWSER_QUEUE_WAIT_S` (default 5), then gets 429 `busy`. |
| `BROWSER_MAX_TIMEOUT_S` | 180 | Upper bound for a request's `timeout_s`. The deadline covers queueing; there is a 2 s grace for partial results, then 504. |
| `BROWSER_MAX_RESPONSE_BYTES` | 5 MiB | Response cap. html and text are each capped at half of it, and a screenshot over the cap returns 413. |
| `BROWSER_MAX_DOWNLOAD_BYTES` | 64 MiB | Bytes one request may pull through the egress proxy. |
| `BROWSER_MAX_REQUEST_BYTES` | 256 KiB | Request body cap (413). Fixed in code. |
| `BROWSER_SEARCH_BACKENDS` / `BROWSER_SEARCH_TIMEOUT_S` | legacy list / 10 | ddgs engines and per-search timeout. |
| `FLARESOLVERR_URL` | empty | Turns on the `flaresolverr` engine. Requires the two proxy settings below. |
| `BROWSER_EGRESS_PROXY_LISTEN` / `BROWSER_EGRESS_PROXY_URL` | loopback | Where the egress proxy listens (e.g. `0.0.0.0:8091`) and the URL FlareSolverr uses to reach it (`http://browser:8091`). |
| `BROWSER_EGRESS_ALLOW_CIDRS` | empty | **Test only.** Exempts ranges from the egress block list. The service refuses to start with it unless `APP_ENV=test`, and metadata IPs stay blocked even then. |

The Go client adds 10 s on top of `timeout_s` to its own deadline to cover queueing and the grace period. It
caps success bodies at 16 MiB (`Client.MaxBodyBytes`). It never retries on its own; callers can use
`browserclient.Retryable(err)` to decide whether a retry makes sense.

## Egress safety (SSRF)

**How it works: resolve-and-pin through an authenticated in-process forward proxy.**

- Chromium is started with `--proxy-server` pointing at the proxy and `--proxy-bypass-list=<-loopback>`, with
  QUIC disabled and non-proxied WebRTC UDP disabled. Each browser context gets its own per-request proxy
  credentials.
- Because a proxy is set, Chromium never resolves DNS itself. Every connection reaches the proxy as
  `CONNECT host:port` or as an absolute-form `http://` request. That covers navigations, every redirect hop,
  subresources, iframes, fetch/XHR and WebSockets.
- The proxy resolves the host once and refuses it if any of its addresses is not globally routable. That
  includes RFC 1918, loopback, link-local and 169.254.169.254, CGNAT, ULA, multicast, reserved, NAT64, 6to4,
  Teredo, IPv4-mapped forms, and fd00:ec2::254 / 100.100.100.200. It then dials the vetted IP itself, which
  closes the DNS-rebinding window.
- Names with no dot (compose service names), `localhost`, and `*.internal`, `*.local` and similar names are
  refused before any DNS lookup. So are IP literals in any encoding.
- curl_cffi (impersonate) and ddgs (search) use the same proxy with per-request credentials.
- FlareSolverr runs its own browser in another container, so each request passes it the proxy address plus
  that request's credentials. The credentials are revoked when the call returns. The service also DNS-vets the
  target and the final URL as a second layer, and refuses to start with `FLARESOLVERR_URL` set unless the proxy
  is reachable from FlareSolverr.
- When a navigation or redirect is refused, the service returns 403 `egress_blocked`. When a subresource is
  refused, the page still loads and the refusal is counted in `blocked_requests`.
- Each request also has a download byte cap, and its tunnels are torn down when the request ends.

**Network.** Compose attaches `browser` to two networks:

- `browser_net` is `internal: true`, so it has no route out. Only api-go and worker-go share it.
- `browser_egress` is the service's own internet path, and nothing else joins it.

No port is published. postgres, redis and the Python api are not on either network, and the proxy refuses
private addresses anyway.

## Operate

```bash
# token: openssl rand -hex 32  ->  BROWSER_SERVICE_TOKEN in .env (same value for api-go/worker-go)
docker compose --profile browser up -d browser
docker compose --profile browser ps browser          # healthy = /healthz answers
docker compose exec api-go /api -healthcheck          # (Go side unaffected)
```

- **Image:** `apps/browser-service/Dockerfile`. It uses python:3.13-slim with the Playwright headless Chromium
  shell (~1.3 GB) and runs as uid 10001. Compose runs it read-only, with tmpfs `/tmp` and `/home/browser`,
  `cap_drop: ALL`, `no-new-privileges`, `shm_size` 1g and `mem_limit` `${BROWSER_MEM_LIMIT:-2g}`.
- **Smoke test:** `apps/browser-service/scripts/smoke_container.sh` builds and runs the image with no published
  port. It renders a local page inside the container and checks that a redirect to 127.0.0.2 and the metadata
  IP are refused. It also checks that the container refuses to start without a token and runs as uid 10001.
- **Logs** contain hosts and error class names only. They never contain tokens, cookies, proxy credentials,
  page content or full URLs.
- **Contract:** after a model change, run
  `uv run python -m browser_service.openapi_export > ../backend-go/internal/browserclient/testdata/openapi.json`
  and update the Go types. The Python test `test_checked_in_openapi_matches_the_service` and the Go
  `TestContract*` tests each fail on drift.

## Not in the service (Go owns these)

- gosom `gmaps_service_client`: a plain REST client to another sidecar. There is no browser work in it, and the
  egress guard would block that internal host.
- Search result cache and per-host backoff, keyed search engines (they need API keys), proxy-pool rotation.
- Company-name extraction and lead mapping from hits, dedup, scoring.

## Rollback

Nothing calls the service yet; the callers are switched over by later packages behind their own flags. To roll
back, stop the profile (`docker compose stop browser`). The legacy Python scrapers are unchanged.
