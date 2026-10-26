# Runbook: leads, signals and SSE events cutover to Go (plan 05)

Go serves the lead CRUD/read/export/dedup routes, the signal routes and the `GET /api/events` SSE stream
next to FastAPI, using the same percentage split as plans 02-04 (`$auth_upstream`, `infra/nginx/go-split.conf`).
Ramp and rollback are config edits plus `nginx -s reload`. 41 route decorators exist in
`routers/leads.py` + `routers/signals.py`: 20 are ported, 21 stay on Python. `TestEveryPythonLeadSignalRouteIsPortedOrRetained`
fails when a route is in neither list.

## Prerequisites (do not skip)

1. Plans 01-04 cutovers are complete and verified (`sqlite-to-postgres.md`, `api-cutover.md`, `settings-cutover.md`,
   plan 04 workbook runbook): `api-go`, `go-split.conf` and `go-routes.conf` are in place behind nginx.
2. `PG_LEAD_STORE=true` (the default) on **both** `api` and `api-go`. Go supports only the Postgres lead store:
   with `PG_LEAD_STORE=false`, `RegisterLeads/RegisterSignals/RegisterEvents` register nothing, log a warning and
   every ported path answers 404 from Go. Never raise the canary in that state.
3. Redis reachable from `api-go` (`REDIS_URL`), the same Redis the Python API and worker publish to. Go never
   emits progress events itself; it only relays `leadgen:progress:<workspace_id>`. Without Redis `/api/events`
   still answers 200 but sends heartbeats only, so the UI loses live progress. Check with `redis-cli PUBSUB CHANNELS 'leadgen:progress:*'`
   during a collection.
4. `SSE_MAX_STREAMS` on `api-go` (default 1000; 503 `{"detail":...}` once full). Size it to the expected
   concurrent browser tabs and the file-descriptor limit of the container. Python has no cap.
   Also `SSE_MAX_STREAMS_PER_USER` (default 10) and `SSE_MAX_STREAMS_PER_WORKSPACE` (default 200): beyond either,
   429 `Too many event streams for this user|workspace`, so one user cannot starve other tenants; nginx adds
   `limit_conn sse_per_ip 20` (429). `SSE_MAX_LIFETIME_SECONDS` (default 1800) closes a stream after that long so
   `EventSource` reconnects and re-authenticates (token expiry, removed members and SSO changes take effect then).
   On SIGTERM open streams end at once, so a Go restart no longer waits out the 30 s grace.
5. The Python worker is running and consumes `executor='python'` jobs: `POST /api/leads/bulk-enrich` and
   `POST /api/signals/scan` only enqueue `bulk_enrich` / `signal_scan`; the scrapers stay in Python.
6. The `?token=` access token travels in the query string and nginx cannot redact it from `error_log` request
   lines; the `/api/events` location logs only `crit` and above, but treat every nginx `error_log` (and the Python
   uvicorn access log) as sensitive and keep it out of shared log pipelines.
7. The nginx build contains the plan-05 section of `go-routes.conf` and the `sse_noquery` log format in `go-split.conf`.

## Routing summary

| Path | Upstream |
| --- | --- |
| `GET /api/leads`, `/api/leads/`, `/api/signals/`, `GET /api/stats`, `/api/filters`, `POST /api/lead`, `GET/PUT/DELETE /api/lead/{id}`, `/api/lead/{id}/timeline|similar|status`, `GET /api/export/csv`, `POST /api/leads/dedup`, `/dedup/merge`, `/bulk-enrich`, `GET /api/signals`, `/analytics`, `/counts`, `POST /api/signals/scan`, `/mark-read` | Go for the canary percentage, else Python |
| `GET /api/events` | Go for the canary percentage, else Python; SSE settings below |
| 21 retained routes (below) | always Python, at every percentage |

All plan-05 locations are exact (`=`) or regex locations, so none can shadow a retained path, which falls
through to `location /api/`. Responses carry `X-Upstream: go|py` on ported paths only.

### `/api/events` specifics

- `proxy_buffering off; proxy_cache off; proxy_http_version 1.1; proxy_read_timeout 1h; add_header X-Accel-Buffering no always;`
  and `Connection ""` towards the upstream (no WebSocket upgrade headers on an SSE stream). The Go server has
  no `WriteTimeout`; it flushes after every frame and heartbeats every 15 s of silence, well inside the read timeout.
- No stickiness is needed: both stacks subscribe to the same Redis channel and emit the same bytes
  (`data: {...}\n\n`, Python `json.dumps` encoding). A client that is re-bucketed simply reconnects.
- The browser sends the access token as `?token=`. The location logs with `access_log /dev/stdout sse_noquery;`
  (`$uri`, no query string), so the token is not in the nginx access log. nginx *error* log lines for a failed
  upstream still print the full request line (`request: "GET /api/events?token=..."`): do not ship `error_log`
  at `info`/`debug` to a shared sink, and treat tokens as short-lived (they are access tokens).
  `scripts/nginx-routing-check.sh` asserts the log line carries no token.

## Retained on Python (21 routes, `testdata/contract/retained_leads_routes.txt`)

| Routes | Why |
| --- | --- |
| `POST /api/lead/{id}/enrich` (SSE, OSM/DDG), `POST /api/collect`, `GET /api/v2/search/unified`, `POST /api/v2/scraper/scrape`, `POST /api/leads/verify-email`, `POST /api/leads/tech-stack` | the six `@limiter.limit` routes (30, 20, 60, 30, 60, 30 per minute): slowapi counters live in Python. Go's `internal/ratelimit` is not used here |
| `GET /api/jobs`, `/api/jobs/{id}`, `/stages`, `/leads`, `/events`, `POST /jobs/{id}/cancel`, `DELETE /jobs/{id}`, `POST /jobs/{id}/retry`, `GET /api/system-stats` | they read the per-workspace SQLite `leads.db` job ledger, which Go cannot open (D1) |
| `POST /api/leads/score`, `/leads/domain-intel` | Python scoring/intel services (enrichment moves with plan 06) |
| `GET /api/signals/sources` | Python signal-source registry (plan 06) |
| `GET/POST /api/workspaces`, `DELETE /api/workspaces/{ws_id}` in `leads.py` | **dead code in Python**: `main.py` does not mount `leads_router.workspace_router` ("intentionally NOT mounted"); the canonical `/api/workspaces` is `ws_manager_router`, ported in plan 02 (`^~ /api/workspaces` location). They are listed so the coverage test accounts for every decorator. |

The Go binary registers none of them (`TestRetainedRoutesNotRegisteredInGo`).

## Canary ramp (matches `go-split.conf`)

The single operator knob is the `go` line in `infra/nginx/go-split.conf`, shipped commented out (0%, all Python).
Sticky per client (hash of Authorization header + client address).

1. 5%: uncomment `5%  go;`, then `docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload`.
   Watch for 24 h: Go 5xx rate, `X-Upstream: go` share, Redis subscriber count, SSE stream gauge vs `SSE_MAX_STREAMS`,
   `jobs` rows for `bulk_enrich` / `signal_scan` being picked up by the worker.
2. 25%: change the line to `25% go;`, reload, watch again.
3. 100%: `100% go;`, reload. Python keeps serving the 21 retained routes.
4. **Rollback** at any step: comment the `go` line out (back to `* py;`), `nginx -t`, reload. There is no data
   migration to undo: Go and Python write the same Postgres tables with the same row shapes. In-flight SSE
   streams on Go end at reload (`worker_shutdown_timeout`) and clients reconnect to Python.

`nginx` rejects a literal `0%`, which is why rollback means commenting the line out.

## Verification

- Before any ramp: `scripts/nginx-routing-check.sh` (throwaway nginx, real config): ported paths `py` at 0% and `go` at 100%,
  all 21 retained paths `py` at both, `/api/events` has the SSE directives and the token is absent from its log line.
- `cd apps/backend-go && make vet test test-integration` (integration needs `TEST_DATABASE_URL`, `TEST_REDIS_URL`).
- At each percentage: `curl -si -H "Authorization: Bearer $T" https://<host>/api/leads?limit=1 | grep -i x-upstream`; compare the
  body between stacks for the same token (`jq -S`); `curl -N "https://<host>/api/events?token=$T"` shows a
  `data: {"type": "heartbeat"}` frame within 15 s and, during a collection, progress frames.
- A foreign `workspace_id` on `/api/events` must answer 403 `Workspace access denied`; a lead id of another
  workspace answers 404 (never 403).

## Deliberate deviations from FastAPI

Contract parity is the release bar; these are the documented exceptions (per-route detail in
`apps/backend-go/testdata/contract/README.md`).

- **D1** Job ledger routes, collection and enrich stay on Python until the ledger moves to Postgres.
- **D2** Crash-class inputs answer 422 instead of a Python 500: wrong-typed or null-into-NOT-NULL values in
  `PUT /api/lead/{id}`, negative `limit`/`offset` on `/api/leads`, NaN/Infinity-shaped cursors; non-integer
  `dedup/merge` ids answer 400 `master_id and duplicate_ids required`. Nothing is written. `/api/leads` `limit` is capped at 10,000.
- **D3** `SSE_MAX_STREAMS` (default 1000) returns 503 when full; without Redis the stream carries heartbeats only.
- **D4** `dedup/merge` is one transaction (master update + duplicate deletes); Python committed each statement separately.
- **D5** Dedup normalisation, blocking keys, scores and clusters are Unicode-correct and match Python
  (`\b`, `.lower()`, code-point lengths, decimal rounding), verified by Python-generated vectors.
- **D6** `GET /api/signals/counts` lists type names sorted ascending (Python's group-by order is unspecified).
- **Analytics bucketing**: `GET /api/signals/analytics` keeps `CAST(created_at / 86400 AS INTEGER)` verbatim. Postgres
  rounds (not floors) that cast, so activity after 12:00 UTC lands in the next day's bucket, as it already does
  in Python on the Postgres store; documented, not fixed, to keep byte parity.
- Signal cursors that are not urlsafe-base64 JSON `[created_at, id]` answer 422 `invalid signal cursor` (or an empty
  page), never 500.

## Open-decision outcomes

All six defaults (D1-D6) were applied on 2026-10-26 under the standing rules; nothing is pending.
