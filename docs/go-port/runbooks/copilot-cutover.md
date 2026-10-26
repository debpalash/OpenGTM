# Runbook: copilot chat and person intel cutover to Go (plan 12)

Go serves 12 routes next to FastAPI: 7 under `/api/copilotkit` (chat SSE `POST /api/copilotkit`, `GET|POST /info`, conversations list/get/delete,
`GET /memories`) and 5 person intel routes (`POST /api/person/enrich`, `GET /api/person/history`, `GET|DELETE /api/person/{id}`,
`GET /ws/person/enrich`). **No route is retained on Python** (`apps/backend-go/testdata/contract/retained_copilot_routes.txt`). What stays on Python
is the LLM provider call, the eight pre-route branches and ~21 of the 25 agent tools (collection, enrichment, workbooks, plans, outreach drafting,
AmbitionBox, people research), reached by Go over `PY_WORKER_URL` (`POST /internal/copilot/{preroute,llm,tool,describe,system-prompt}`).
Only `search_leads`, `get_lead_detail`, `get_lead_stats` and `update_lead_status` execute in Go. Deviations: `apps/backend-go/testdata/contract/README.md`,
section "Plan 12 (copilot chat and person intel)".

## Prerequisites

1. Plan 01 tables backfilled: `chat_conversations`, `chat_messages`, `chat_memories`, `chat_tool_approvals` (Postgres, RLS) and `person_intel`;
   Go requires `PG_LEAD_STORE=true` (see 3).
2. `PY_WORKER_URL` and `PY_WORKER_TOKEN` set on `api-go` (same bearer as plan 06). Without a worker Go registers the REST history routes but **not
   chat** (it logs `copilot chat route not registered: PyWorker is not configured`).
3. **Flag state decides whether Go answers or 404s, and nginx cannot see it.** Routes register only when `COPILOT_GO_ENABLED=true` **and**
   `PG_LEAD_STORE=true`. With either off Go registers nothing and logs a warning; a request that reaches Go then gets 404 (it does **not** fall
   through to Python). Never raise the canary above 0% unless both are on and the startup log has no `not registered` line (`copilot routes not registered` for the flag state, and `copilot chat route not registered: PyWorker is not configured` for a missing worker: then the other 6 routes are served by Go and `POST /api/copilotkit` answers 404/405).
4. **Python replicas must run the Task 2 code** (`routers/internal_copilot.py`, `services/copilot/*`): during the canary both stacks must answer
   identically, and Go calls those endpoints. Deploy the Python image first, verify `POST /internal/copilot/system-prompt` answers from the worker
   (internal network only; `/internal/` is 404 at the gateway).
5. **`openmemory` must NOT be installed on the Python workers (D3).** Python prefers it over the builtin store and its data is a private SQLite file
   Go cannot read; with it installed chats on the two stacks would recall different memories. If it was installed, its old memories stop being
   recalled after cutover. Confirm with `GET /api/copilotkit/memories` on both stacks for a test user (same list = builtin store in use).
6. `CHAT_REQUIRE_AUTH` **equal on both stacks** (unset defaults to `PG_LEAD_STORE`). Self-host (`false`) binds to the active workspace; with no
   resolvable workspace Go answers 403 where Python used an empty id. Go refuses to start with `CHAT_REQUIRE_AUTH=false` set explicitly and a non-dev
   `APP_ENV` unless `COPILOT_ALLOW_ANONYMOUS_CHAT=true` (anonymous chat lets anyone who reaches nginx approve dangerous tools).
7. Limits (defaults in parentheses): `COPILOT_MAX_BODY_BYTES` (1 MiB), `COPILOT_MAX_STREAMS_PER_USER` (3), `COPILOT_TURN_TIMEOUT_SECONDS` (900),
   `COPILOT_TOOL_TIMEOUT_SECONDS` (120), `SSE_MAX_STREAMS` (1000, **one pool shared with `/api/events`**), `WS_MAX_MESSAGE_BYTES`. The Go API must be
   restarted after any flag change (config is read at boot). The integer limits have hard ranges and a value outside them (or a non-integer) **fails
   startup**: `COPILOT_MAX_BODY_BYTES` 1..64 MiB, `COPILOT_MAX_STREAMS_PER_USER` 1..1000, `COPILOT_TURN_TIMEOUT_SECONDS` and `COPILOT_TOOL_TIMEOUT_SECONDS` 1..86400.
8. nginx build contains `# ===== BEGIN plan 12: copilot chat and person intel =====` in `go-routes.conf` and `scripts/nginx-routing-check.sh`
   passes at 0% and 100%. Both knobs in `go-split.conf` (`$go_bucket` and `$go_ws_bucket`) must carry the **same** percentage.
9. `/ws/person/enrich` had no gateway location before this plan (it fell through to the SPA) and no frontend code uses it. The gateway answers **404 unless
   the request is routed to Go** (never to the Python handler: it has no deadline, no caps and echoes `str(exc)`). It is admin-only with a query token;
   the location logs without the query string.

## Routing

- `/api/copilotkit`, `/info`, `/conversations`, `/conversations/{id}`, `/memories`: bucket keyed per **workspace** (`X-Workspace-Id`, else the client
  key), so a conversation is not split across stacks mid-turn. Both stacks read the same tables, so a split is safe but confusing.
- `/api/person/{enrich,history,id}` and `/ws/person/enrich`: client bucket (the WebSocket only exists on the Go side; Python-bucket clients get 404).
- Lookalikes, unknown subpaths and trailing-slash variants (`/api/copilotkit/`, `/api/person/`, `/api/personx`) stay on Python at every percentage.
- Chat location: `proxy_buffering off`, `proxy_read_timeout 1h`, `proxy_http_version 1.1`, `X-Accel-Buffering` honoured (never ignored);
  `proxy_request_buffering` is **not** set (small body, bounded by Go). WebSocket location: `Upgrade`/`Connection` headers, `proxy_read_timeout 300s`.
  Responses carry `X-Upstream: go|py` for spot checks.

## Canary steps

0. Deploy Python replicas with the Task 2 code and Go with `COPILOT_GO_ENABLED=true`, `PG_LEAD_STORE=true`, worker settings. Canary stays 0%.
   Smoke through the gateway with a canary user at 0% (all `X-Upstream: py`), then hit `api-go` directly: `GET /api/copilotkit/info`, a one-line chat,
   `GET /api/copilotkit/conversations`, an approval round trip on a **test workspace** (propose a dangerous tool, approve it, confirm it ran once).
1. **5%**: edit both lines in `go-split.conf`, `docker compose exec nginx nginx -t && docker compose exec nginx nginx -s reload`. Watch 30 min.
2. **25%**: watch 1 h across a business-hours peak.
3. **100%**: watch a full day. Keep Python replicas up for rollback.

Go criteria between steps: all metrics below flat versus the Python baseline, a manual approval round trip OK on each stack, a person enrichment
over the WebSocket OK for an admin.

## What to watch

| signal | where | investigate when |
|---|---|---|
| chat 5xx (`POST /api/copilotkit`, `X-Upstream: go`) | nginx access log, Go logs | any sustained rise; 503 `AI worker unavailable` = worker down or `PY_WORKER_*` wrong |
| stream error frames (`{"error": ...}` then `[DONE]`) | Go log `copilot turn ended with an error frame` (reason, conversation; never content) | above Python's baseline; `AI provider stream interrupted` = worker/provider cut |
| RPC p95 first-frame latency (Go -> worker `llm`) | worker access log, Go log | p95 climbing: worker saturation; one extra RPC per tool round is expected (D2) |
| tool timeouts (`Tool '<name>' failed` / 408) | Go log `copilot python tool failed`, tool results | a tool above `COPILOT_TOOL_TIMEOUT_SECONDS`; dangerous tools are never retried |
| cap 429s (`Too many active chat streams`, WS 429/503) | nginx and Go | raise `COPILOT_MAX_STREAMS_PER_USER` / `SSE_MAX_STREAMS` only after checking for a leak (active streams must fall to 0 when idle) |
| approval anomalies | `chat_tool_approvals` rows stuck `pending` older than 1 h, or a dangerous tool running with no row in status `approve` (the consumed statuses are `approve` and `deny`) | any (should be none) |
| 413/422 on chat | nginx and Go | new client behaviour (messages > 200, body > 1 MiB, non-string content); legitimate clients should see none |

## Rollback

Set the canary to 0% (comment the `go` lines in `go-split.conf`, `nginx -t`, reload). **No data migration**: both stacks use the same tables.
In-flight Go streams are not cut by the reload: each ends on its own and is bounded by the **whole-turn** deadline (`COPILOT_TURN_TIMEOUT_SECONDS`, default 15 min,
covering pre-route, context assembly, approval resume and tool rounds), after which the client gets `Request timed out`; Python serves new requests. A turn cancelled mid-tool leaves its approval
consumed, so a resubmit after rollback is denied ("invalid, expired, already consumed"): the user asks again. To remove Go's registration set
`COPILOT_GO_ENABLED=false` and restart. If memories diverge after a rollback, check `openmemory` (prerequisite 5).

## Deviations to expect (full list in the README)

- Hardening answers where Python 500s: 422 for malformed chat bodies, 413 for oversized ones, 400 for more than **20** `approved_tool_calls`, 503 when
  the worker cannot be reached before the response starts, 429 for stream caps; non-object tool args/results are read as empty.
- A turn cancelled mid-tool leaves its approval consumed; persistence failures are logged and do not abort the stream; non-string `content` frames are
  ignored; nothing is stored after a client disconnect (parity).
- `person_intel` is **global and admin-gated** (Python parity): every admin sees all profiles.
- Person WebSocket: disconnect cancels and saves nothing (Python saves), one enrichment per admin, 120 s deadline, 30 s save timeout (includes the pool
  wait), generic error text instead of `str(exc)`, pre-accept auth failure is an HTTP 403 handshake refusal.
- Self-host with no workspace: 403.
- Go-only bounds (Python has none): `search_leads` limit capped at 100; at most 20 dangerous calls per model round (`Too many tool calls requested`); approved
  dangerous tools are never retried after a dropped worker connection; person enrichment (REST and WS) is one run per admin and draws on the shared
  `SSE_MAX_STREAMS` pool; a non-JSON tool result is wrapped as `{"raw": ...}` and the turn continues (Python ends it).
- Not fixed, Python bug (issue #114): the scraper behind the deep person scrape has a DNS-rebinding gap in its SSRF guard and an uncapped fast-path body;
  Go inherits it through `person_intel_deep` until the scraper is ported or fixed.
- No recorded WebSocket fixture exists for `/ws/person/enrich` (live scraping); it is covered by integration tests.

## Verification commands

```sh
cd apps/backend-go && make vet test test-integration          # needs TEST_DATABASE_URL (see Makefile)
go test -race -tags integration -run 'TestChaos' ./internal/copilot   # chaos: worker cut/stall, disconnect, panic, concurrent approvals
scripts/nginx-routing-check.sh                                  # 12 paths follow the bucket at 0%, 50% (workspace stickiness) and 100%
```
