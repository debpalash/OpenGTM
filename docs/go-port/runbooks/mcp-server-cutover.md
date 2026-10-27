# Runbook: MCP server cutover to Go (plan 13)

> **Historical (plan 15).** OpenGTM now runs without the Python backend: the Python api, worker, scheduler
> and seed services, the root `Dockerfile` and the nginx canary (`go-split.conf`, `go-routes.conf`) are gone, and
> every Go path is on (`GO_ALL_NATIVE=true` in compose). This document records the side-by-side period. Current
> operation: `docs/go-port/runbooks/docker-compose.md`; final cutover: `docs/go-port/runbooks/15-cutover.md`.

`cmd/mcp` (binary `opengtm-mcp`) replaces `python -m apps.mcp.server`: the same JSON-RPC surface and the same
15 tools over stdio and over a loopback SSE/HTTP endpoint. Wire parity with Python is the release bar; every
deliberate difference is a numbered deviation in `apps/backend-go/testdata/contract/README.md` ("MCP (plan 13)").

## Routing: no nginx change

The Python SSE server binds loopback and is not proxied by nginx today, and the Go one is the same. There is
nothing to add to `infra/nginx`. `/api/mcp/tokens` (token CRUD) is a different thing: it is plan 02, served by
the API, and `scripts/nginx-routing-check.sh` already asserts it is unaffected. If an operator exposes the SSE
port through their own reverse proxy, that proxy must keep forwarding the `Authorization` header and the
`Host` header must still name a loopback host unless `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true` (see below).

## Build and run

```
cd apps/backend-go && make build-mcp          # bin/opengtm-mcp
OPENGTM_MCP_TOKEN=ycp_... DATABASE_URL=... opengtm-mcp              # stdio
DATABASE_URL=... opengtm-mcp --sse 3100                              # SSE on OPENGTM_MCP_SSE_HOST (127.0.0.1)
```

`DATABASE_URL` must be the runtime role (a superuser or BYPASSRLS role is refused at start). Logs go to stderr;
stdout carries protocol bytes only. Exit status: 0 on clean EOF or SIGINT/SIGTERM, 1 on any startup refusal,
config error, non-loopback SSE bind without the override, or a stdio session whose token was revoked or expired
(D8, within `MCP_STDIO_REVALIDATE_SECONDS`).

SSE: `--sse [port]` (default 3100), host from `OPENGTM_MCP_SSE_HOST` (fallback `YUPCHA_MCP_SSE_HOST`, default
`127.0.0.1`). A non-loopback host is refused unless `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK=true`; with the override
the Host/Origin loopback check is skipped too, so put a reverse proxy with TLS in front and nothing else.
`GET /sse` is the only unauthenticated route; every `POST /message` needs `Authorization: Bearer <token>`.

## Environment

| Variable | Default | Notes |
| --- | --- | --- |
| `OPENGTM_MCP_TOKEN` (fallback `YUPCHA_MCP_TOKEN`) | none | stdio token; required when `MCP_REQUIRE_AUTH` |
| `MCP_REQUIRE_AUTH` | true | |
| `MCP_WRITE_ENABLED` | false | enable LAST (see order below) |
| `MCP_TOKEN_TTL_DAYS` | 90 | |
| `MCP_MAX_WRITES_PER_DAY` | 0 | 0 = unlimited |
| `MCP_MAX_ENRICH_PER_DAY` | 0 | new; caps enrich_company, verify_email, get_hiring_signals |
| `MCP_TOOL_TIMEOUT_SECONDS` | 120 | new; per tool call |
| `MCP_STDIO_REVALIDATE_SECONDS` | 30 | new; stdio revocation latency |
| `AUTOMATIONS_ENABLED` | false | gate for `create_automation` |
| `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` | false | |
| `AUTOMATIONS_MAX_RULES_PER_WS` | 50 | |
| `EGRESS_ALLOW_HOSTS` | empty | operator allow-list, comma separated `host` or `host:port`; the only way the email cascade may reach a private self-hosted Reacher |
| `REACHER_URL`, `REACHER_TIMEOUT` | unset | global operator config only (`app_settings` then env), never per workspace; `REACHER_ENABLED` resolves per workspace |
| `PY_WORKER_URL`, `PY_WORKER_TOKEN` | unset | Python worker RPC for `enrich_company`, `get_hiring_signals`, `score_lead` |
| `SECRETS_PROVIDER`, `SECRETS_MASTER_KEY`, `SECRET_KEY`, `VAULT_*`, `APP_ENV` | as the API | provider credentials resolve per workspace through the secrets store |
| `LEADGEN_SMTP_VERIFY`, `LEADGEN_SMTP_HELO`, `LEADGEN_SMTP_MAIL_FROM` | as Python | SMTP probe settings |

## Prerequisites and tools not yet available

- Plan 06 deployed and `PY_WORKER_URL` reachable: without it `enrich_company`, `get_hiring_signals` and
  `score_lead` answer an enrichment-backend error. `verify_email` runs in Go (cascade, optional Reacher).
- Plans 08 (automations) and 09 (outreach) provide the adapters for `enroll_leads`, `send_email` and
  `create_automation`. Until they land these three answer `{"error": "<tool> is not available in this build"}`.
  Deployments that use them must keep the Python MCP server (or at least those clients) until then.
- Plans 04/05 (lead writes and reads) and the workbook CRUD (`create_workbook`, `delete_workbook`) are in Go.

## Cutover

1. Mint a test token (`POST /api/mcp/tokens`) with `leads:read` and call `tools/list` and `find_leads` against
   both servers; compare. Keep `MCP_WRITE_ENABLED` off: write tools are then absent from `tools/list` and refuse.
2. Switch one client. stdio config: `"command": "opengtm-mcp"` with `"env": {"OPENGTM_MCP_TOKEN": "ycp_..."}`
   (rollback form: `"command": "python", "args": ["-m", "apps.mcp.server"]`, same env var).
3. SSE service swap: replace the unit running `python -m apps.mcp.server --sse` with `opengtm-mcp --sse 3100`
   on the same host and port; clients keep their URL and bearer token.
4. Watch `mcp_audit_log` (cost-bearing read tools are audited now: `ok`/`error`/`capped`) and stderr.
5. Enable `MCP_WRITE_ENABLED=true` last, after read traffic has been clean; then `AUTOMATIONS_ENABLED` once plan 08
   is in.

## Rollback

Nothing is migrated: tokens, audit and cap tables are the same Postgres tables Python uses. Roll back by pointing
clients (or the service unit) at `python -m apps.mcp.server` again. Keep the Python MCP server installed until
every tool you use is ported.

## Deviations summary

D2 `isError` stays false; D3 argument errors and bounds; D4 malformed JSON-RPC answers errors and unknown
notifications get no reply; D5 reserve, write and settle share one transaction (a failed write frees its slot and
the key stays retryable); D6 nested audit redaction, loopback Host/Origin guard, failed-auth rate limit; D7 audit and
optional cap on cost-bearing read tools; D8 stdio revocation within the interval (exit 1); plus the items in the
contract README (stats bucket order, NaN, `enabled: null`, actions bounds, `update_lead` check order, post-commit
cancel, falsy idempotency key, IDNA2003 approximation).
- Post-port fix wave: a tool result is an error (`isError` true) when it is a JSON object with an `error` key (same rule as the write audit).
