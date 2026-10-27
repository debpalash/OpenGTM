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
| `POST /v1/search` | One keyless ddgs query (text or news) with the caller's backends, region, safesearch and timelimit. Failures come back as `error`: `no_results`, `rate_limited`, `timeout` or `failed` (rate_limited and timeout are the legacy block signals). |
| `POST /v1/extract` | Main text and metadata (trafilatura, legacy content_extract) plus markdown (markdownify) from given HTML or a fetched URL. Its body cap is `BROWSER_MAX_EXTRACT_BYTES`. |
| `POST /v1/holehe` | Per-site holehe results for one email (first 50 modules, concurrency 10, as legacy). The verdict mapping stays in Go. |
| `POST /v1/staffspy` | A company's LinkedIn roster. Off (503 `engine_unavailable`) unless `BROWSER_STAFFSPY_ENABLED`. The LinkedIn cookies come with each request; nothing is written to disk. |
| `GET /v1/stats` | This process's counters: calls, ok and error codes per op, search failures by class, egress sessions, refusals and upstream failures. Per replica, not aggregated. |

Every page, search, holehe and staffspy request takes an optional `upstream_proxy` (http, socks5 or socks4 URL).
Go picks it from `PROXY_LIST` or the proxy manager; the service never stores or logs it. See "Upstream proxies" below.

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
| `BROWSER_EGRESS_PORTS` | 80,443 | Destination ports the egress proxy may open. The upstream proxy's own port is not limited. |
| `BROWSER_MAX_EXTRACT_BYTES` | 8 MiB | Request body cap for `/v1/extract` only (it takes page HTML). |
| `BROWSER_CHROMIUM_SANDBOX` | true | Chromium's own sandbox. In a container it needs the security options under "Chromium sandbox". With it on and those options missing, Chromium does not start and browser endpoints answer 503. |
| `BROWSER_MAPS_STEALTH` | false | Google Maps through patchright (stealth Playwright). Off matches what legacy production ran: patchright was the optional `browser` extra and the image only had Playwright. |
| `BROWSER_STAFFSPY_ENABLED` | false | `/v1/staffspy` (LinkedIn ToS; legacy `STAFFSPY_ENABLED`). |
| `BROWSER_PLAYWRIGHT_WS` / `BROWSER_PATCHRIGHT_WS` | empty | Split mode only, set by the entrypoint: loopback `ws://127.0.0.1:<port>/<random path>` of the Playwright servers that run Chromium as uid 10002. |
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
- Only ports 80 and 443 are opened unless `BROWSER_EGRESS_PORTS` lists more.
- FlareSolverr fails closed: its answer is refused (`upstream_error`) when the session's proxy carried no traffic
  (a build that ignored the proxy object fetched from its own network), and refused (`egress_blocked`) when the
  proxy refused any destination during the solve. FlareSolverr logs request bodies, so the per-request proxy
  credentials it receives end up in its log; they are revoked when the call returns.
- `locale`, `user_agent` and cookie names/values are validated (no CR/LF or control characters) because they end
  up in request headers.
- When a navigation or redirect is refused, the service returns 403 `egress_blocked`. When a subresource is
  refused, the page still loads and the refusal is counted in `blocked_requests`.
- Each request also has a download byte cap, and its tunnels are torn down when the request ends.

**Upstream proxies.** When a request names an `upstream_proxy`, the egress proxy still resolves and vets the target
first, then opens the tunnel through the upstream (HTTP CONNECT, SOCKS5 with optional user/password, or SOCKS4),
asking it for the vetted IP, never the name, so the far side cannot be rebound either. The upstream's own address is
vetted by the same policy: a private or metadata proxy address is refused (502 with `x-opengtm-egress:
upstream_blocked`). A refused or failed handshake is 502 `upstream_failed`, and the API answers `upstream_error`, so
Go can mark the proxy bad. The credentials live only in that request's session.

**Chromium sandbox and process split (review M1).**

- Chromium runs with its sandbox on. Docker's defaults block it in two ways: the default seccomp profile refuses the
  user/pid/net namespaces it creates, and the default AppArmor profile refuses its `/proc/self/uid_map` write. Run
  the container with `seccomp-chromium.json`, which is moby's v26 default profile plus `clone`, `unshare`, `setns`
  and `chroot`, and with AppArmor unconfined:

  ```yaml
  browser:
    security_opt:
      - no-new-privileges:true
      - seccomp=./apps/browser-service/seccomp-chromium.json
      - apparmor=unconfined
  ```

  `cap_drop: ALL` stays. A stricter custom AppArmor profile that only allows the uid_map write would be a host-level
  follow-up.
- Before Playwright starts, the service removes `BROWSER_SERVICE_TOKEN` from its environment, so the Playwright
  driver and Chromium never inherit it. It also makes itself non-dumpable (`PR_SET_DUMPABLE=0`), so a same-uid
  process cannot read its `/proc/<pid>/environ` or memory. Chromium is launched with a minimal environment.
- **Split mode** (Chromium as its own uid): start the container as root with only `SETUID` and `SETGID`. The
  entrypoint then starts a Playwright server (and a patchright one when `BROWSER_MAPS_STEALTH` is on) as uid 10002 on
  loopback with a random path, and the service as uid 10001. Every process drops all capabilities and sets
  no-new-privs.

  ```yaml
  browser:
    user: "0"
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID]
  ```

  Without these the image runs as uid 10001 and Chromium is a child of the service; it is still sandboxed and still
  has no token.
- `scripts/smoke_container.sh` runs both modes with the compose hardening and the security options above. It checks
  that Chromium has no `--no-sandbox` flag, runs as the expected uid, and that the service's environment cannot be
  read from another process.
- **Known limit (not split, plan 15 WP-RETIRE-INFRA):** the egress proxy runs inside the browser container, so a
  Chromium that escapes its sandbox has a direct route on `browser_egress` and can reach api-go and worker-go on
  `browser_net` (their ports only; postgres and redis are not on that network). Moving the proxy into its own
  container is not a compose-only change: the proxy authenticates each request with per-request session credentials
  and enforces that session's byte cap and upstream proxy, all held in the service's memory (`EgressProxy.open_session`
  / `close_session` in `egress.py`). A separate proxy container needs a small session-registration protocol between
  the two processes (or signed, self-describing credentials). Then the browser container would join only an internal
  network to the proxy and the Go services, and the proxy alone would join `browser_egress`. Until then the sandbox,
  split-mode uids, the dropped capabilities and the read-only root are the boundary.

**Network.** Compose attaches `browser` to two networks:

- `browser_net` is `internal: true`, so it has no route out. Only the Go services (api-go, worker-go, mcp) share it.
- `browser_egress` is the service's own internet path, and nothing else joins it.

No port is published. postgres and redis are not on either network, and the proxy refuses
private addresses anyway.

## Operate

```bash
# token: openssl rand -hex 32  ->  BROWSER_SERVICE_TOKEN in .env (same value for api-go/worker-go)
# part of the default stack; compose sets the security_opt shown under "Chromium sandbox" above
docker compose up -d browser
docker compose ps browser                             # healthy = /healthz answers
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

## Not in the service (Go owns these, in `internal/leadweb`)

- gosom `gmaps_service_client`: `leadweb.GMaps`, a plain REST client. Its egress client allow-lists only the
  `GMAPS_SCRAPER_URL` host.
- The search chain: `leadweb.Searcher` (the legacy `_ResilientDDGS`). It handles the result cache (tenant-salted,
  TTL 600 s, empty TTL 60 s, LRU 2000), per-host backoff, direct-then-proxy attempts through `/v1/search`, and the
  keyed fallback engines (SerpAPI, Bing, Google CSE, Brave, Serping, Serply), whose API keys never reach the
  service. It also covers the SearXNG pool and direct path that providers used (`WebSearch`).
- Proxies: `PROXY_LIST` rotation (`ProxyPool`) and the proxy manager (`ProxyManager`, `PROXY_MANAGER_URL`). Go
  passes the chosen proxy per request.
- The page fetch facade, `leadweb.Fetcher` (the legacy StealthClient). It handles per-domain rate limiting and the
  circuit breaker, then escalates from impersonate to the browser to FlareSolverr.
- HTML helpers (emails, phones, challenge detection, links, meta, the website contact scan), company-name
  extraction, lead mapping, dedup and scoring.

## Rollback

The Go services are the only callers. Without the service (`docker compose stop browser`), browser-backed work
(rendering, Google Maps, scrape routes, search through `/v1/search`) fails with `browser service is not available`;
keyed search engines called from Go keep working.
